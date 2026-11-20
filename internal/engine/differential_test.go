package engine_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sql/corpus"
	sqloracle "maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/store/simstore"
)

var (
	seed      = flag.Uint64("seed", 0, "first seed of the sequences of statements")
	sequences = flag.Int("sequences", 300, "sequences of statements of TestDifferential")
	length    = flag.Int("length", 40, "statements in each sequence of TestDifferential")
	scanAll   = flag.Bool("scanall", false, "read every row of its table in each query of TestDifferential and FuzzDifferential_core, and not the rows by the key")
)

// pinned fails the test unless got, a number of texts that a full run counts, is want. Such a number
// is pinned when it tells how much of a test escapes the comparison with SQLite, so that a change that
// lets more escape cannot pass unseen.
func pinned(t *testing.T, what string, want, got int) {
	t.Helper()
	if got != want {
		t.Errorf("%v: %v, but the test pins %v. Change the pinned number only on purpose, and say why in the commit", what, got, want)
	}
}

// atDefault reports whether each flag of names has its default value, for which the pinned numbers
// hold.
func atDefault(names ...string) bool {
	for _, name := range names {
		if f := flag.Lookup(name); f.Value.String() != f.DefValue {
			return false
		}
	}
	return true
}

// slow reports whether the tests run with -short, the race detector, or the address sanitizer, which
// make them several times slower: the tests of many sequences then run a tenth of them.
func slow() bool {
	return testing.Short() || raceEnabled || asanEnabled
}

// answer of a statement: what is compared between the engine and SQLite.
type answer struct {
	// err is SQLite's error as "code/extended: message", "not supported", "no statement",
	// [wrongArguments], or "".
	err string
	// message of the engine's error, which is not compared with SQLite.
	message string
	// offset of SQLite's error, or of the engine's, as [sqlerr.Error] has it, and 0 for another
	// error. It is not in the text of the answer: SQLite does not give the same offset on every
	// connection, so only the tests that know it compare it, such as pin, which compares the
	// answers of a fresh connection to SQLite and of the engine after the same statements.
	offset int
	// columns of a query, each as its name and declared type, and its rows, each as its values.
	columns string
	rows    []string
	// null reports whether a value of the rows is NULL. It is not in the text of the answer, which
	// holds each NULL.
	null bool
}

func (a answer) String() string {
	if a.err != "" {
		return a.err
	}
	return fmt.Sprintf("columns %q, rows %q", a.columns, a.rows)
}

// oracle is SQLite, on a database in memory, which runs the statements that the engine runs.
type oracle struct {
	c *sqlite.Conn
	// counts gives SQLite's count of changes and its last insert rowid.
	counts *sqlite.Stmt
}

func newOracle(t testing.TB) *oracle {
	t.Helper()
	c, err := sqlite.Open(":memory:")
	is.NotError(t, err)
	counts, err := c.Prepare("select changes(), last_insert_rowid()")
	is.NotError(t, err)
	o := &oracle{c: c, counts: counts}
	t.Cleanup(o.close)
	return o
}

// close the database of the oracle. Closing it again does nothing.
func (o *oracle) close() {
	_ = o.counts.Finalize()
	_ = o.c.Close()
}

// changes is SQLite's count of changes, and rowid its last insert rowid.
func (o *oracle) changes(t testing.TB) (changes, rowid int64) {
	t.Helper()
	ok, err := o.counts.Step()
	is.NotError(t, err)
	is.True(t, ok)
	changes, rowid = o.counts.ColumnInt64(0), o.counts.ColumnInt64(1)
	is.NotError(t, o.counts.Reset())
	return changes, rowid
}

// prepare src, and finalize its statement without a step, so that nothing changes. It gives SQLite's
// error, or nil. Any other error fails the test.
func (o *oracle) prepare(t testing.TB, src string) *sqlite.Error {
	t.Helper()
	s, _, err := o.c.PrepareFirst(src)
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se
	}
	if err != nil {
		t.Fatalf("%q: SQLite gives %v, which is not one of its errors", src, err)
	}
	if s != nil {
		_ = s.Finalize()
	}
	return nil
}

// sqliteText of an error of the binding.
func sqliteText(t testing.TB, err error) string {
	t.Helper()
	var se *sqlite.Error
	if !errors.As(err, &se) {
		t.Fatalf("SQLite gives %v, which is not one of its errors", err)
	}
	return fmt.Sprint(se.Code.Primary(), "/", int(se.Code), ": ", se.Message)
}

// sqliteAnswer of an error of the binding, with its offset.
func sqliteAnswer(t testing.TB, err error) answer {
	t.Helper()
	a := answer{err: sqliteText(t, err)}
	var se *sqlite.Error
	if errors.As(err, &se) {
		a.offset = se.Offset
	}
	return a
}

// run the statement of src, as sqlite3_prepare_v2 and sqlite3_step run it, with args bound to its
// parameters by position: each an int64, a float64, a string, or a []byte.
func (o *oracle) run(t testing.TB, src string, args ...any) answer {
	t.Helper()
	s, _, err := o.c.PrepareFirst(src)
	if err != nil {
		return sqliteAnswer(t, err)
	}
	if s == nil {
		return answer{err: "no statement"}
	}
	defer func() { _ = s.Finalize() }()
	for j, a := range args {
		var err error
		switch a := a.(type) {
		case int64:
			err = s.BindInt64(j+1, a)
		case float64:
			err = s.BindDouble(j+1, a)
		case string:
			err = s.BindText(j+1, a)
		case []byte:
			err = s.BindBlob(j+1, a)
		case nil:
			err = s.BindNull(j + 1)
		default:
			t.Fatalf("%q: an argument of type %T for SQLite", src, a)
		}
		if err != nil {
			return sqliteAnswer(t, err)
		}
	}
	var a answer
	var columns []string
	for i := range s.ColumnCount() {
		columns = append(columns, s.ColumnName(i)+" "+s.ColumnDeclType(i))
	}
	a.columns = strings.Join(columns, ", ")
	for {
		ok, err := s.Step()
		if err != nil {
			return sqliteAnswer(t, err)
		}
		if !ok {
			return a
		}
		values := make([]string, s.ColumnCount())
		for i := range values {
			switch typ := s.ColumnType(i); typ {
			case sqlite.TypeInteger:
				values[i] = fmt.Sprint(s.ColumnInt64(i))
			case sqlite.TypeFloat:
				values[i] = realText(s.ColumnDouble(i))
			case sqlite.TypeText:
				values[i] = strconv.Quote(s.ColumnText(i))
			case sqlite.TypeBlob:
				values[i] = blobText(s.ColumnBlob(i))
			case sqlite.TypeNull:
				values[i], a.null = "null", true
			default:
				t.Fatalf("SQLite gives a value of type %v", typ)
			}
		}
		a.rows = append(a.rows, strings.Join(values, ","))
	}
}

// state of the database in SQLite: each table, in the order of the names as bytes, with its
// columns, as [columnText] gives them, and its rows in the order of select *.
func (o *oracle) state(t testing.TB) string {
	t.Helper()
	s, err := o.c.Prepare("select name from sqlite_schema where type = 'table' order by name collate binary")
	is.NotError(t, err)
	var names []string
	for {
		ok, err := s.Step()
		is.NotError(t, err)
		if !ok {
			break
		}
		names = append(names, s.ColumnText(0))
	}
	is.NotError(t, s.Finalize())
	var b strings.Builder
	for _, name := range names {
		a := o.run(t, "select * from "+stmttest.Quote(name))
		_, columns := o.columns(t, name)
		var cs []string
		for _, c := range columns {
			cs = append(cs, columnText(c.Name, c.Type, c.Nullable))
		}
		fmt.Fprintf(&b, "%v (%v): %v\n", name, strings.Join(cs, ", "), strings.Join(a.rows, " "))
	}
	return b.String()
}

// columnText of a column of a table in the text of a state: its name and its declared type, and
// NULL after them if it takes NULL.
func columnText(name, typ string, nullable bool) string {
	if nullable {
		return name + " " + typ + " NULL"
	}
	return name + " " + typ
}

// errText of an error of the engine, as [answer] has it. Any other error fails the test.
func errText(t testing.TB, src string, err error) string {
	t.Helper()
	var se *sqlerr.Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &se):
		return fmt.Sprint(se.Code, "/", se.ExtendedCode, ": ", se.Message)
	case errors.Is(err, stmt.ErrNotSupported):
		return "not supported"
	case errors.Is(err, engine.ErrNoStatement):
		return "no statement"
	case errors.Is(err, engine.ErrArguments):
		return wrongArguments
	}
	t.Fatalf("%q: the engine gives %v", src, err)
	return ""
}

// wrongArguments is the err of an [answer] of the engine's error of another number of arguments
// than the statement has parameters.
const wrongArguments = "wrong number of arguments"

// errAnswer of an error of the engine, with its message.
func errAnswer(t testing.TB, src string, err error) answer {
	t.Helper()
	a := answer{err: errText(t, src, err), message: err.Error()}
	var se *sqlerr.Error
	if errors.As(err, &se) {
		a.offset = se.Offset
	}
	return a
}

// integers as values of the engine.
func integers(vs []int64) []*s4qlpb.Value {
	var values []*s4qlpb.Value
	for _, v := range vs {
		values = append(values, s4qlpb.Value_builder{Integer: new(v)}.Build())
	}
	return values
}

// values of the arguments args of a call of [stmttest.Sequence], as the engine gets them from the
// driver: an integer for a Go integer that fits a signed 64-bit integer, and a real for a float32,
// which the driver converts, the value of a [sql.NamedArg], whose name the driver refuses before the
// engine, text, a real, and a blob as they are, and for every other argument a value that the driver
// never hands the engine: NULL for nil, and a value of no kind for a type that has no value of the
// engine and for a nil []byte.
func values(args []any) []*s4qlpb.Value {
	var vs []*s4qlpb.Value
	for _, a := range unnamed(args) {
		if v, ok := stmttest.Integers([]any{a}); ok {
			vs = append(vs, s4qlpb.Value_builder{Integer: new(v[0])}.Build())
			continue
		}
		var b s4qlpb.Value_builder
		switch a := converted(a).(type) {
		case string:
			b.Text = new(a)
		case float64:
			b.Real = new(a)
		case []byte:
			b.Blob = a
		case nil:
			b.Null = &s4qlpb.Null{}
		}
		vs = append(vs, b.Build())
	}
	return vs
}

// converted argument a, as the driver converts a float32 to a float64. Every other argument is as
// the driver hands it to the engine, or one that it refuses by its type.
func converted(a any) any {
	if f, ok := a.(float32); ok {
		return float64(f)
	}
	return a
}

// unnamed args: the value of each [sql.NamedArg] in its place.
func unnamed(args []any) []any {
	var as []any
	for _, a := range args {
		if n, ok := a.(sql.NamedArg); ok {
			a = n.Value
		}
		as = append(as, a)
	}
	return as
}

// valueText of a value of the engine, as [oracle.run] writes SQLite's. It fails the test for a blob
// that is nil and for a value of no kind.
func valueText(t testing.TB, v *s4qlpb.Value) string {
	t.Helper()
	if v.HasBlob() && v.GetBlob() == nil {
		t.Fatal("the engine gives a blob that is nil")
	}
	text, ok := valueString(v)
	if !ok {
		t.Fatalf("the engine gives a value of kind %v", v.WhichKind())
	}
	return text
}

// valueString of a value of the engine, as [oracle.run] writes SQLite's, and whether it has a kind.
func valueString(v *s4qlpb.Value) (string, bool) {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return fmt.Sprint(v.GetInteger()), true
	case s4qlpb.Value_Real_case:
		return realText(v.GetReal()), true
	case s4qlpb.Value_Text_case:
		return strconv.Quote(v.GetText()), true
	case s4qlpb.Value_Blob_case:
		return blobText(v.GetBlob()), true
	case s4qlpb.Value_Null_case:
		return "null", true
	}
	return fmt.Sprint("a value of kind ", v.WhichKind()), false
}

// realText of a real: "real " and the shortest digits that give it, so that two reals have the same
// text only if they have the same bits, and -0.0 is "real -0".
func realText(r float64) string {
	return "real " + strconv.FormatFloat(r, 'g', -1, 64)
}

// blobText of a blob, as a blob literal.
func blobText(b []byte) string {
	return "x'" + hex.EncodeToString(b) + "'"
}

// declType of a column of the type t, as SQLite reports it.
func declType(t s4qlpb.Type) string {
	return map[s4qlpb.Type]string{
		s4qlpb.Type_TYPE_INT: "INT", s4qlpb.Type_TYPE_INTEGER: "INTEGER", s4qlpb.Type_TYPE_REAL: "REAL",
		s4qlpb.Type_TYPE_TEXT: "TEXT", s4qlpb.Type_TYPE_BLOB: "BLOB",
	}[t]
}

// instance of the engine on a database of the storage core: a store in memory, and an instance on
// a host with real goroutines.
type instance struct {
	t     testing.TB
	host  *sim.Free
	store *simstore.Store
	dir   string
	// caches counts the caches made, each a file of its own.
	caches int
	path   string
	d      *core.DB
	e      *engine.Engine
	// c is the connection of the engine that the statements of the instance run on.
	c *engine.Conn
	// changes is SQLite's count of changes on the connection, as the results of the engine give it.
	changes int64
	// memory is the state of an engine on a database in memory, which has no core, with each value
	// as its argument writes it, or nil.
	memory func(text func(*s4qlpb.Value) string) string
	// coreLog is the logger of the core, or nil.
	coreLog *slog.Logger
	// limit is the length limit of the cache, 0 for SQLite's default.
	limit int
}

// newMemoryInstance of the engine on a database in memory.
func newMemoryInstance(t testing.TB) *instance {
	e, state := engine.NewMemory(sim.NewFree(sim.Buggify{}))
	return &instance{t: t, e: e, c: e.Conn(), memory: state}
}

func newInstance(t testing.TB, options engine.Options) *instance {
	t.Helper()
	return newInstanceLogged(t, options, nil)
}

// newInstanceLogged is [newInstance] with a logger of the core.
func newInstanceLogged(t testing.TB, options engine.Options, coreLog *slog.Logger) *instance {
	t.Helper()
	return newInstanceLimited(t, options, coreLog, 0)
}

// newInstanceLimited is [newInstanceLogged] with the length limit of the cache limit, 0 for SQLite's
// default.
func newInstanceLimited(t testing.TB, options engine.Options, coreLog *slog.Logger, limit int) *instance {
	t.Helper()
	h := sim.NewFree(sim.Buggify{})
	i := &instance{t: t, host: h, store: simstore.New(h, simstore.Options{}), dir: t.TempDir(), coreLog: coreLog, limit: limit}
	i.open(true, options)
	t.Cleanup(func() {
		i.close()
		is.NotError(t, i.c.Close(context.WithoutCancel(t.Context())))
	})
	return i
}

// open an instance of the database, with an empty cache if empty, and with the last one otherwise.
func (i *instance) open(empty bool, options engine.Options) {
	i.t.Helper()
	if empty || i.path == "" {
		i.caches++
		i.path = filepath.Join(i.dir, fmt.Sprint("cache", i.caches, ".db"))
	}
	ctx := context.WithoutCancel(i.t.Context())
	// A slow machine must not make the cache busy or the lease run out: the waits for both are far
	// longer than any statement takes.
	coreOptions := core.Options{BusyTimeout: time.Minute, Lease: time.Hour, Logger: i.coreLog, LengthLimit: i.limit}
	d, err := core.Open(ctx, i.host, i.store.Client(fmt.Sprint("client-", i.host.Uint64()), nil), i.path, "db", coreOptions)
	is.NotError(i.t, err)
	is.NotError(i.t, d.Wait(ctx))
	// The connection of the instance that was closed before, if there was one, is closed after its
	// instance, which has closed the reader of the connection.
	if i.c != nil {
		is.NotError(i.t, i.c.Close(ctx))
	}
	i.d, i.e = d, engine.New(d, i.host, options)
	i.c = i.e.Conn()
}

// close the instance, which releases the lease. Its connection stays, for the statements that a
// test runs on an instance that is closed.
func (i *instance) close() {
	if i.d == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(i.t.Context()), time.Minute)
	defer cancel()
	is.NotError(i.t, i.d.Close(ctx))
	i.d = nil
}

// restart the instance: close it, and open a new one, with an empty cache if empty.
func (i *instance) restart(empty bool, options engine.Options) {
	i.t.Helper()
	i.close()
	i.open(empty, options)
}

// query src with args through [engine.Conn.Query].
func (i *instance) query(src string, args ...int64) answer {
	i.t.Helper()
	return i.queryValues(src, integers(args))
}

// queryValues is [instance.query] with values of the engine.
func (i *instance) queryValues(src string, args []*s4qlpb.Value) answer {
	i.t.Helper()
	rows, err := i.c.Query(i.t.Context(), src, args...)
	if rows != nil {
		if res := rows.Result(); res.SetsChanges {
			i.changes = res.Changes
		}
	}
	if err != nil {
		return errAnswer(i.t, src, err)
	}
	var a answer
	var columns []string
	for _, c := range rows.Columns() {
		columns = append(columns, c.Name+" "+c.DeclType)
	}
	a.columns = strings.Join(columns, ", ")
	for row, err := range rows.All() {
		if err != nil {
			return answer{err: errText(i.t, src, err)}
		}
		values := make([]string, len(row))
		for j, v := range row {
			values[j] = valueText(i.t, v)
			a.null = a.null || v.HasNull()
		}
		a.rows = append(a.rows, strings.Join(values, ","))
	}
	return a
}

// exec src with args through [engine.Conn.Exec], and keep SQLite's count of changes as its result
// says.
func (i *instance) exec(src string, args ...int64) answer {
	i.t.Helper()
	return i.execValues(src, integers(args))
}

// execValues is [instance.exec] with values of the engine.
func (i *instance) execValues(src string, args []*s4qlpb.Value) answer {
	i.t.Helper()
	res, err := i.c.Exec(i.t.Context(), src, args...)
	if res.SetsChanges {
		i.changes = res.Changes
	}
	if err != nil {
		return errAnswer(i.t, src, err)
	}
	return answer{}
}

// state of the database in the engine's instance, as [oracle.state] gives it, read from the core.
func (i *instance) state() string {
	i.t.Helper()
	if i.memory != nil {
		return i.memory(func(v *s4qlpb.Value) string { return valueText(i.t, v) })
	}
	ctx := i.t.Context()
	rd, err := i.d.Reader(ctx)
	is.NotError(i.t, err)
	defer func() { is.NotError(i.t, rd.Close(ctx)) }()
	r, err := rd.Read(ctx)
	is.NotError(i.t, err)
	defer func() { is.NotError(i.t, r.Close(ctx)) }()
	tables, err := r.Tables(ctx)
	is.NotError(i.t, err)
	var b strings.Builder
	for _, ct := range tables {
		var columns []string
		for _, c := range ct.GetColumns() {
			columns = append(columns, columnText(c.GetName(), declType(c.GetType()), c.GetNullable()))
		}
		var rows []string
		for row, err := range r.Scan(ctx, ct.GetTable()) {
			is.NotError(i.t, err)
			values := make([]string, len(row))
			for j, v := range row {
				values[j] = valueText(i.t, v)
			}
			rows = append(rows, strings.Join(values, ","))
		}
		fmt.Fprintf(&b, "%v (%v): %v\n", ct.GetTable(), strings.Join(columns, ", "), strings.Join(rows, " "))
	}
	return b.String()
}

// sequence of random statements of slice 1, as [stmttest.Sequence] gives them.
type sequence = stmttest.Sequence

func newSequence(t testing.TB, r stmttest.Rand) *sequence {
	t.Helper()
	q, err := stmttest.NewSequence(r)
	is.NotError(t, err)
	return q
}

// newSequenceFrom r, after [newSequence] has succeeded once.
func newSequenceFrom(r stmttest.Rand) *sequence {
	q, err := stmttest.NewSequence(r)
	if err != nil {
		panic(err)
	}
	return q
}

// tally of the outcomes of the statements of a differential test, which counts nothing if it is
// nil.
type tally map[string]int

func (y tally) add(src string, a answer) {
	if y == nil {
		return
	}
	kind := "other"
	if s, _ := stmt.Check(src); s != nil {
		kind = strings.ToLower(strings.TrimPrefix(fmt.Sprintf("%T", s), "*stmt."))
		if s.Parameters() > 0 {
			kind += " with a parameter"
		}
	}
	switch {
	case a.err == "":
		y[kind+" ok"]++
	case strings.Contains(a.err, ": "):
		y[kind+" "+messageKind(a.err)]++
	default:
		y[kind+" "+a.err]++
	}
}

// excused counts a text of the kind that the tests excuse from the comparison with SQLite, under
// "excused: " and the kind.
func (y tally) excused(kind string) {
	if y != nil {
		y["excused: "+kind]++
	}
}

// acceptedStartedTable is the kind of a text that [startedTable] accepts, and knownGapSuffix ends the
// kind of a text of a [knownGap], after the gap.
const (
	acceptedStartedTable = "already exists after the start of a table, accepted"
	knownGapSuffix       = ", a known gap"
)

// The kinds of the calls that the tests do not run in SQLite, because s4ql refuses them by an
// agreed rule where SQLite runs them, and whose answer they check against that rule instead, by
// [checkArguments].
const (
	// excusedArguments is a call with another number of arguments than its statement has
	// parameters: SQLite binds NULL to a parameter without an argument, and refuses an argument for
	// a parameter that the statement does not have only when it is bound.
	excusedArguments = "the wrong number of arguments, checked against s4ql's rule"
	// excusedArgument is a call with an argument that no column holds, which SQLite binds: NULL, and
	// a value of no kind, for a Go type that the driver does not convert.
	excusedArgument = "an argument that no column holds, checked against s4ql's rule"
	// excusedType is an insert of a value of another type than its column, which SQLite converts or
	// refuses with an error of its own.
	excusedType = "a value of another type than its column, checked against s4ql's rule"
	// excusedLong is an insert of a literal that is longer than the length limit, which SQLite meets
	// only with a limit below the length of its text, and then refuses later or turns into NULL.
	excusedLong = "a literal longer than the length limit, checked against s4ql's rule"
	// excusedLongChange is an update or a delete with a literal longer than the length limit, in SET
	// or in a condition, which s4ql refuses with SQLite's `string or blob too big` after the errors of
	// the names, whether the statement finds a row or not, where SQLite gives the error only when it
	// changes a row, and turns a blob literal into NULL: the known difference of docs/todo.md. See
	// [pair.checkLength].
	excusedLongChange = "an update or a delete with a literal longer than the length limit, checked against s4ql's rule"
	// excusedRecordSlip is an update of two rows or more that s4ql refuses with SQLite's `string or
	// blob too big` for a record of a row, longer than the length limit by SQLite's measure of that row
	// alone, with no error of an earlier row, which SQLite stores where its allocator leaves room: the
	// known difference of
	// docs/todo.md. See [pair.recordSlips]. Its count depends on the platform, so no test pins it.
	excusedRecordSlip = "an update of rows of which SQLite does not measure every record, checked by an update of each row alone"
	// excusedLongCondition is a select with a literal in a condition that is longer than the length
	// limit, which s4ql refuses with SQLite's `string or blob too big`, and for which SQLite gives
	// rows: no rows for a blob literal, which it turns into NULL, and for a string literal on the
	// upper bound of the key whose search reaches no row. See [pair.longCondition].
	excusedLongCondition = "a literal in a condition longer than the length limit, for which SQLite gives rows, checked against s4ql's rule"
)

// knownGap of the pass after the parser, as [stmttest.KnownGap] gives it, that the engine's answer
// got and SQLite's answer want for the text src fall in, or "". Its tests excuse such a text in the
// same way.
func knownGap(src string, got, want answer) string {
	prefix := fmt.Sprint(sqlerr.CodeError, "/", sqlerr.CodeError, ": ")
	m, ok := strings.CutPrefix(got.err, prefix)
	w, wok := strings.CutPrefix(want.err, prefix)
	if !ok || !wok {
		return ""
	}
	return stmttest.KnownGap(src, m, w)
}

// startedTable reports whether the engine's answer got and SQLite's answer want for the text src
// differ in the one way that the tests accept, which [stmttest.ExistsAfterStart] describes: SQLite
// gives that a table exists, and the engine gives what SQLite gives for src on the empty database
// empty.
func startedTable(t testing.TB, empty *oracle, src string, got, want answer) bool {
	t.Helper()
	w, ok := strings.CutPrefix(want.err, fmt.Sprint(sqlerr.CodeError, "/", sqlerr.CodeError, ": "))
	if !ok || !stmttest.ExistsAfterStart(src, w) {
		return false
	}
	return got.String() == empty.run(t, src).String()
}

// messageKind of an error's text: its message without the names.
func messageKind(err string) string {
	for _, kind := range []string{
		"already exists", "no such table", "has no column named", "no such column", "values were supplied", " values for ", "duplicate column name",
		"too many columns on", "UNIQUE constraint failed", "NOT NULL constraint failed", "syntax error", "incomplete input", "string or blob too big",
		"attempt to write a readonly database",
	} {
		if strings.Contains(err, kind) {
			return kind
		}
	}
	return err
}

// differential runs the sequence of n statements from seed on the engine and on SQLite, and compares
// after each statement what each gives, the count of changes, and the last insert rowid, and at the
// end, and after each restart, every table. With restarts, the instance is closed and opened again
// at random points, with an empty cache or with its cache. The cache and SQLite have the length
// limit limit, or SQLite's default for 0.
func differential(t testing.TB, seed uint64, n int, restarts bool, limit int, y tally) {
	t.Helper()
	// The instance closes at the end of the sequence, so that a test of many sequences does not keep
	// the files of each open until it ends.
	i := newInstanceLimited(t, generatedOptions(), nil, limit)
	defer i.close()
	differentialOn(t, seed, n, restarts, y, i)
}

// generatedOptions of the engine of the generated tests: every query reads every row of its table
// with -scanall, and the rows by the key otherwise.
func generatedOptions() engine.Options {
	var o engine.Options
	if *scanAll {
		o.ScanAll()
	}
	return o
}

// differentialOn runs the sequence of n statements from seed on the instance i, as [differential]
// does.
func differentialOn(t testing.TB, seed uint64, n int, restarts bool, y tally, i *instance) {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, 0xe1e))
	q := newSequence(t, r)
	p := newPair(t, i, y)
	// The other instance closes at the end of the sequence too, as [differential] closes i.
	defer p.other.close()
	var log []string
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("seed %v, after\n%v\n%v", seed, strings.Join(log, "\n"), fmt.Sprintf(format, args...))
	}
	for range n {
		if restarts && r.IntN(10) == 0 {
			empty := r.IntN(2) == 0
			log = append(log, fmt.Sprint("-- restart, with an empty cache: ", empty))
			i.restart(empty, generatedOptions())
			if got, want := i.state(), p.o.state(t); got != want {
				fail("after the restart the engine holds\n%v\nand SQLite\n%v", got, want)
			}
		}
		c := q.Next()
		log = append(log, fmt.Sprintf("%v -- with %#v", c.Src, c.Args))
		if failure := p.step(c); failure != "" {
			fail("%v", failure)
		}
	}
	want := p.o.state(t)
	if got := i.state(); got != want {
		fail("at the end the engine holds\n%v\nand SQLite\n%v", got, want)
	}
	if got := p.other.state(); got != want {
		fail("at the end the other engine, which reads the rows the other way, holds\n%v\nand SQLite\n%v", got, want)
	}
}

// pair of the engine's instance i and SQLite o, which run the same statements.
type pair struct {
	t testing.TB
	i *instance
	o *oracle
	// empty is SQLite on an empty database, which prepares each text that the engine does not
	// support, and changes nothing.
	empty *oracle
	// prev is SQLite's count of changes after the statement before.
	prev int64
	// readOnly is set while the statements run in a read-only transaction.
	readOnly bool
	// other is a second instance of the engine, of the kind of i, that runs every call of the pair and
	// reads the rows the other way: every row where the engine of i reads by the key, and by the key
	// where it reads every row. See [pair.bothWays].
	other *instance
	// last is the engine's answer of the last call of [pair.step].
	last answer
	y    tally
}

// newPair of the instance i and SQLite, with the length limit of i, and a second instance of the
// engine of the kind of i, on a database in memory or on the core with the length limit of i.
func newPair(t testing.TB, i *instance, y tally) *pair {
	t.Helper()
	p := &pair{t: t, i: i, o: newOracle(t), empty: newOracle(t), y: y}
	if i.memory != nil {
		p.other = newMemoryInstance(t)
	} else {
		p.other = newInstanceLimited(t, engine.Options{}, nil, i.limit)
	}
	t.Cleanup(p.o.close)
	t.Cleanup(p.empty.close)
	if i.limit > 0 {
		is.NotError(t, p.o.c.SetLengthLimit(i.limit))
	}
	return p
}

// step runs the call c on the engine, through Query if c.Query and through Exec otherwise, with the
// values of its arguments, and on SQLite, and compares what each gives, the count of changes, and the
// last insert rowid. A text that the engine does not support is not run in SQLite: it must not be a
// statement of slices 1 and 2, which c.Supported says it is, and SQLite must prepare it on an empty
// database without a syntax error. A call that the engine refuses for its arguments is not run in
// SQLite either, and must be refused as s4ql's rule says, which [checkArguments] checks. step gives
// what differs, or "".
func (p *pair) step(c stmttest.Call) string {
	t, i, o := p.t, p.i, p.o
	t.Helper()
	src := c.Src
	var got answer
	if c.Query {
		got = i.queryValues(src, values(c.Args))
	} else {
		got = i.execValues(src, values(c.Args))
	}
	p.last = got
	p.y.add(src, got)
	p.reach(c, got)
	if failure := p.bothWays(c, got); failure != "" {
		return failure
	}
	if kind, failure := checkArguments(c, got); kind != "" {
		if failure == "" {
			p.y.excused(kind)
		}
		return failure
	}
	// What is left has a value that a column can hold for each parameter, or a text that the engine
	// refuses before its arguments, a syntax error or no statement, which SQLite refuses before it
	// binds them, so that it runs in SQLite without them.
	args := oracleArgs(c.Args)
	if s, err := stmt.Check(src); err != nil || s == nil {
		args = nil
	}
	if kind, failure := p.checkLength(c, got); kind != "" {
		if failure == "" {
			p.y.excused(kind)
		}
		return failure
	}
	if kind, failure := p.checkType(c, args, got); kind != "" {
		if failure == "" {
			p.y.excused(kind)
		}
		return failure
	}
	if got.err == "not supported" {
		// A statement of the generator and every statement that the pass supports are supported, but
		// for the refusals of their arguments and of the types of their values, which are checked
		// above, and a text that is not supported is one that SQLite accepts: SQLite prepares it on an
		// empty database without a syntax error.
		if s, err := stmt.Check(src); c.Supported || err == nil && s != nil {
			return fmt.Sprintf("%q: the engine does not support a statement that the pass supports: %v", src, got.message)
		}
		if m := syntaxOfNotSupported(t, p.empty, src); m != "" {
			return fmt.Sprintf("%q: the engine does not support it, and SQLite gives %v", src, m)
		}
		return ""
	}
	if got.err == wrongArguments {
		return fmt.Sprintf("%q with %v arguments: the engine gives %q", src, len(c.Args), got.message)
	}
	before := p.prev
	// An update that the engine refuses for a record that is too long runs in SQLite in a savepoint,
	// which is rolled back, since SQLite does not measure every record of a statement that changes
	// many rows: see [pair.recordSlips].
	u, _ := stmt.Check(src)
	update, mayslip := u.(*stmt.Update)
	mayslip = mayslip && got.err == tooBig && p.i.limit > 0
	if mayslip {
		is.Equal(t, "", o.run(t, "savepoint s4ql_record").err)
	}
	want := o.run(t, src, args...)
	if mayslip {
		is.Equal(t, "", o.run(t, "rollback to s4ql_record").err)
		is.Equal(t, "", o.run(t, "release s4ql_record").err)
		if (want.err == "" || strings.Contains(want.err, "UNIQUE constraint failed")) && p.recordSlips(update, args) {
			p.y.excused(excusedRecordSlip)
			return ""
		}
	}
	if !c.Query {
		want.columns, want.rows = "", nil
	}
	if p.longCondition(src) && want.err == "" && got.err != tooBig {
		// Where SQLite gives rows, as it does for a blob literal, which it turns into NULL, the rule
		// decides alone.
		return fmt.Sprintf("%q with the arguments %v: a literal of a condition is longer than the length limit, and the engine gives %v, and the rule %q", src, args, got, tooBig)
	}
	if got.String() != want.String() {
		gap := knownGap(src, got, want)
		switch {
		case startedTable(t, p.empty, src, got, want):
			p.y.excused(acceptedStartedTable)
		case want.err == "" && got.err == tooBig && p.longCondition(src):
			p.y.excused(excusedLongCondition)
		case gap != "":
			p.y.excused(gap + knownGapSuffix)
		default:
			return fmt.Sprintf("%q with the arguments %v: the engine gives\n%v\nand SQLite\n%v", src, args, got, want)
		}
	}
	// SQLite's count of changes: the engine's result sets it, and a statement that fails keeps it,
	// except one that fails while SQLite steps it, which sets it to 0: a broken constraint, an insert,
	// an update, or a delete in a read-only transaction, and an insert or an update of a row whose
	// record is too long, while its value, which SQLite would refuse when it binds it, is not.
	changes, rowid := o.changes(t)
	p.prev = changes
	recordTooBig := want.err == tooBig && changesRows(src) && !slices.ContainsFunc(args, func(a any) bool { return byteLength(a) > p.i.limit })
	switch {
	case strings.HasPrefix(want.err, fmt.Sprint(sqlerr.CodeConstraint, "/")), strings.HasPrefix(want.err, fmt.Sprint(sqlerr.CodeReadOnly, "/")) && changesRows(src), recordTooBig:
		if changes != 0 {
			return fmt.Sprintf("%q: SQLite counts %v changes after a broken constraint, a refused change, or a record that is too long", src, changes)
		}
	case want.err != "" && changes != before:
		return fmt.Sprintf("%q: SQLite counts %v changes after its error, and %v before", src, changes, before)
	}
	if changes != i.changes {
		return fmt.Sprintf("%q: SQLite counts %v changes, and the engine's results %v", src, changes, i.changes)
	}
	if rowid != 0 {
		return fmt.Sprintf("%q: SQLite's last insert rowid is %v", src, rowid)
	}
	return ""
}

// bothWays runs the call c, whose answer of the engine of the instance is got, on the other instance
// of the pair, through the same method, with every row of a table read, as
// [engine.Engine.SetScanAll] makes it, where the engine of the instance reads the rows by the key, as
// the plan does, and by the key where it reads every row, as an engine of -scanall does. The two must
// give the same answer, message, and count of changes, and after an update or a delete with WHERE
// the same rows of its table, as a select of * gives them on each, so that each test that compares
// the instance with SQLite compares both ways of reading rows, of a select and of the rows that an
// update or a delete changes, also in transactions. The other instance runs every call, so that it
// holds what the instance holds. A second instance, and not a second run on the instance, because an
// update or a delete changes what a second run would find. bothWays counts a select with WHERE in the
// tally under "reached: read both ways", and an update or a delete with WHERE under "reached: changed
// both ways", each also with " in a read-only transaction" in one, and gives what differs, or "".
func (p *pair) bothWays(c stmttest.Call, got answer) string {
	i, other := p.i, p.other
	byKey := !i.e.ScansAll()
	other.e.SetScanAll(byKey)
	var all answer
	if c.Query {
		all = other.queryValues(c.Src, values(c.Args))
	} else {
		all = other.execValues(c.Src, values(c.Args))
	}
	changes, allChanges := i.changes, other.changes
	if !byKey {
		// The instance read every row, as -scanall makes it, and the other instance by the key.
		got, all, changes, allChanges = all, got, allChanges, changes
	}
	if all.String() != got.String() || all.message != got.message || allChanges != changes {
		return fmt.Sprintf("%q with %#v: the engine gives\n%v (%q)\nby the key, and\n%v (%q)\nwith every row read, with %v changes by the key and %v with every row read", c.Src, c.Args, got, got.message, all, all.message, changes, allChanges)
	}
	s, _ := stmt.Check(c.Src)
	var table *stmt.Name
	what := ""
	switch s := s.(type) {
	case *stmt.Select:
		if s.Where != nil {
			what = "read both ways"
		}
	case *stmt.Update:
		if s.Where != nil {
			what, table = "changed both ways", &s.Table
		}
	case *stmt.Delete:
		if s.Where != nil {
			what, table = "changed both ways", &s.Table
		}
	}
	if what == "" {
		return ""
	}
	if p.y != nil {
		p.y["reached: "+what]++
		if p.readOnly {
			p.y["reached: "+what+" in a read-only transaction"]++
		}
	}
	if table == nil {
		return ""
	}
	src := "select * from " + stmttest.Quote(table.Value)
	if rows, allRows := i.queryValues(src, nil), other.queryValues(src, nil); rows.String() != allRows.String() {
		if !byKey {
			rows, allRows = allRows, rows
		}
		return fmt.Sprintf("%q with %#v: after it, the table holds\n%v\nby the key, and\n%v\nwith every row read", c.Src, c.Args, rows, allRows)
	}
	return ""
}

// reach counts in the tally what the call c, whose answer of the engine is got, reaches, under
// "reached: ", on the database of SQLite before the call, as [reach] says.
func (p *pair) reach(c stmttest.Call, got answer) {
	p.t.Helper()
	if p.y != nil {
		reach(p.t, p.o, c, got, p.i.changes, func(what string) { p.y["reached: "+what]++ })
		if p.longCondition(c.Src) {
			p.y["reached: WHERE with a literal longer than the length limit"]++
		}
		kind, in := p.longChange(c.Src)
		if kind != "" {
			p.y["reached: "+kind+" with a literal longer than the length limit in "+in]++
		}
		s, _ := stmt.Check(c.Src)
		if _, ok := s.(*stmt.Update); ok && kind == "" && got.err == tooBig && !slices.ContainsFunc(c.Args, func(a any) bool { return byteLength(a) > p.i.limit }) {
			p.y["reached: update of a row whose record is too long"]++
		}
	}
}

// reach counts with count what the call c, whose answer of the engine is got, reaches on the
// database of o. For an insert: each argument by its kind, as [stmttest.ArgumentKind] names it, each
// literal by its kind, as [stmttest.LiteralKind] names it, a text or a blob of more than 900 bytes,
// and, for an insert into a table that SQLite has, the outcome: for one that SQLite prepares by the
// type of the key column, also for -0.0 in the key that meets a key that exists; what it reaches of
// NULL, as [stmttest.NullCases] gives it; and for a table of several columns by the form of the insert, without
// a list or with a list in the order of the table or in another, and by its faults, as
// [stmttest.Faults] gives them, one or two. For a create table that SQLite prepares, the outcome by
// its number of columns, by the position of its key, for one with two columns of a type and one of
// mixed types, and for one of several columns by whether none, all, or some of the columns outside
// the key take NULL. For a select of a table that SQLite has, the outcome by its form: * by the
// number of columns, names, names in another order than the table's, a name twice, and a name that
// the table does not have, as written and in double quotes; and of * and of names with NULL among
// the values of its rows. For an update or a delete, what [reachChange] counts, with the count of
// changes of the engine after the call, changes.
func reach(t testing.TB, o *oracle, c stmttest.Call, got answer, changes int64, count func(string)) {
	t.Helper()
	outcome := "ok"
	switch {
	case got.err == "not supported" && typeRefusal(c.Src, got.message):
		outcome = "a value of another type"
	case got.err != "":
		outcome = messageKind(got.err)
	}
	s, _ := stmt.Check(c.Src)
	switch s := s.(type) {
	case *stmt.Insert:
		reachInsert(t, o, s, c, outcome, count)
	case *stmt.CreateTable:
		if o.prepare(t, c.Src) != nil {
			return
		}
		count(fmt.Sprint("create table of ", len(s.Columns), " columns ", outcome))
		count(fmt.Sprint("create table with the key at ", s.Key(), " ", outcome))
		kinds := map[string]int{}
		for _, col := range s.Columns {
			kinds[map[stmt.Type]string{stmt.TypeInt: "integer", stmt.TypeInteger: "integer", stmt.TypeReal: "real", stmt.TypeText: "text", stmt.TypeBlob: "blob"}[col.Type]]++
		}
		if len(kinds) > 1 {
			count("create table of mixed types " + outcome)
		}
		if slices.ContainsFunc(slices.Collect(maps.Values(kinds)), func(n int) bool { return n > 1 }) {
			count("create table with two columns of a type " + outcome)
		}
		if len(s.Columns) > 1 {
			nullable := 0
			for _, col := range s.Columns {
				if !col.PrimaryKey && !col.NotNull {
					nullable++
				}
			}
			switch nullable {
			case 0:
				count("create table of which no column takes NULL " + outcome)
			case len(s.Columns) - 1:
				count("create table of which every column outside the key takes NULL " + outcome)
			default:
				count("create table of which some columns outside the key take NULL " + outcome)
			}
		}
	case *stmt.Update, *stmt.Delete:
		reachChange(t, o, s, c, got, outcome, changes, count)
	case *stmt.Select:
		table, columns := o.columns(t, s.Table.Value)
		if columns == nil {
			return
		}
		reachWhere(t, o, s, c, got, outcome, table, columns, count)
		if got.null {
			count(map[bool]string{true: "select * with NULL ", false: "select of names with NULL "}[s.Columns == nil] + outcome)
		}
		if s.Columns == nil {
			count(fmt.Sprint("select * of ", len(columns), " columns ", outcome))
			return
		}
		var positions []int
		for _, name := range s.Columns {
			position := stmttest.ColumnOf(columns, name)
			if position < 0 {
				count(map[bool]string{false: "select of a name that the table does not have ", true: "select of a name in double quotes that the table does not have "}[name.Raw[0] == '"'] + outcome)
				return
			}
			positions = append(positions, position)
		}
		count("select of names " + outcome)
		if !slices.IsSorted(positions) {
			count("select of names in another order " + outcome)
		}
		if len(slices.Compact(slices.Sorted(slices.Values(positions)))) < len(positions) {
			count("select of a name twice " + outcome)
		}
	}
}

// reachWhere counts what the select s of the call c, whose answer of the engine is got and whose
// outcome in the engine is outcome, reaches of WHERE on the database of o, where its table has the
// name table and the columns, as [reach] says: for a select with WHERE, one whose arguments the
// engine refuses, for their number or for one that no column holds; and otherwise each case of
// [stmttest.WhereCases], and for a query that succeeds on a table with rows, whether it gives none,
// some, or all of them.
func reachWhere(t testing.TB, o *oracle, s *stmt.Select, c stmttest.Call, got answer, outcome, table string, columns []stmttest.TableColumn, count func(string)) {
	t.Helper()
	if s.Where == nil {
		return
	}
	args := oracleArgs(c.Args)
	switch {
	case len(args) != s.Parameters():
		count("WHERE with the wrong number of arguments " + outcome)
		return
	case slices.ContainsFunc(args, func(a any) bool { return refusal(a) != "" }):
		count("WHERE with an argument that no column holds " + outcome)
		return
	}
	for _, what := range stmttest.WhereCases(columns, s, args) {
		count(what + " " + outcome)
	}
	// The plan reads the rows by the key as the rule says, which the comparison of the rows cannot see
	// where it reads more rows than it needs.
	if want := stmttest.KeyAccess(columns, s, args); want != "" && got.err != tooBig {
		want = strings.TrimSuffix(want, " that hold no key")
		if access := engine.AccessOf(c.Src, createTableOf(table, columns), values(c.Args)); access != want {
			t.Errorf("%q with %#v: the plan reads %q, and the rule %q", c.Src, c.Args, access, want)
		}
	}
	if !c.Query || got.err != "" {
		return
	}
	n, err := strconv.Atoi(o.run(t, "select count(*) from "+stmttest.Quote(table)).rows[0])
	is.NotError(t, err)
	switch {
	case n == 0:
	case len(got.rows) == 0:
		count("WHERE that gives no row of a table with rows")
	case len(got.rows) == n:
		count("WHERE that gives every row of a table with rows")
	default:
		count("WHERE that gives some rows of a table")
	}
}

// reachChange counts what the update or the delete s of the call c, whose answer of the engine is got,
// whose outcome in the engine is outcome, and after which the engine's count of changes is changes,
// reaches on the database of o, before the call, for a table that SQLite has. An outcome "ok" is "ok,
// rows changed" or "ok, no row found" by the count. It counts the statement by its kind, with or
// without WHERE, and its outcome; one whose arguments the engine refuses, for their number or for one
// that no column holds; and otherwise, for an update, each case of [stmttest.SetCases]; for a WHERE,
// each case of [stmttest.WhereCases] of the select of its conditions, as [stmttest.SelectOf] gives
// it, after the kind, but for those of each condition; for a statement that succeeds on a table with rows, whether it changes none,
// some, or all of them; and it checks the access by the key of the scan that finds the rows, as
// [reachWhere] checks that of a select.
func reachChange(t testing.TB, o *oracle, s stmt.Statement, c stmttest.Call, got answer, outcome string, changes int64, count func(string)) {
	t.Helper()
	kind, name, where := "update", stmt.Name{}, []stmt.Condition(nil)
	switch s := s.(type) {
	case *stmt.Update:
		name, where = s.Table, s.Where
	case *stmt.Delete:
		kind, name, where = "delete", s.Table, s.Where
	}
	table, columns := o.columns(t, name.Value)
	if columns == nil {
		return
	}
	if outcome == "ok" {
		outcome = map[bool]string{true: "ok, no row found", false: "ok, rows changed"}[changes == 0]
	}
	count(kind + map[bool]string{true: " with WHERE ", false: " without WHERE "}[where != nil] + outcome)
	args := oracleArgs(c.Args)
	if len(args) != s.Parameters() || slices.ContainsFunc(args, func(a any) bool { return refusal(a) != "" }) {
		count(kind + " with arguments that s4ql refuses " + outcome)
		return
	}
	missing := false
	if u, ok := s.(*stmt.Update); ok {
		for _, what := range stmttest.SetCases(columns, u, args) {
			count(what + " " + outcome)
			missing = missing || strings.HasPrefix(what, "SET of a column that the table does not have")
		}
	}
	if missing {
		return
	}
	if where != nil {
		sel, selArgs := stmttest.SelectOf(s, args)
		for _, what := range stmttest.WhereCases(columns, sel, selArgs) {
			// The cases of the WHERE as a whole, and not those of each condition, which the selects count.
			if strings.HasPrefix(what, "WHERE ") {
				count(kind + " " + what + " " + outcome)
			}
		}
		// The scan that finds the rows reads them by the key as the rule says, which the comparison of the
		// rows cannot see where it reads more rows than it needs.
		if want := stmttest.KeyAccess(columns, sel, selArgs); want != "" && got.err != tooBig {
			want = strings.TrimSuffix(want, " that hold no key")
			if access := engine.AccessOf(c.Src, createTableOf(table, columns), values(c.Args)); access != want {
				t.Errorf("%q with %#v: the scan of the rows reads %q, and the rule %q", c.Src, c.Args, access, want)
			}
		}
	}
	if got.err != "" {
		return
	}
	n, err := strconv.Atoi(o.run(t, "select count(*) from "+stmttest.Quote(table)).rows[0])
	is.NotError(t, err)
	switch {
	case n == 0:
	case changes == 0:
		count(kind + " that changes no row of a table with rows")
	case changes == int64(n):
		count(kind + " that changes every row of a table with rows")
	default:
		count(kind + " that changes some rows of a table")
	}
}

// createTableOf the table of the name and the columns, as SQLite has them, in the engine's terms.
func createTableOf(name string, columns []stmttest.TableColumn) *s4qlpb.CreateTable {
	ct := s4qlpb.CreateTable_builder{Table: new(name)}
	for i, c := range columns {
		typ := map[string]s4qlpb.Type{"INT": s4qlpb.Type_TYPE_INT, "INTEGER": s4qlpb.Type_TYPE_INTEGER, "REAL": s4qlpb.Type_TYPE_REAL, "TEXT": s4qlpb.Type_TYPE_TEXT, "BLOB": s4qlpb.Type_TYPE_BLOB}[c.Type]
		column := s4qlpb.Column_builder{Name: new(c.Name), Type: new(typ)}
		if c.Nullable {
			column.Nullable = new(true)
		}
		ct.Columns = append(ct.Columns, column.Build())
		if c.PrimaryKey {
			ct.PrimaryKey = []uint32{uint32(i)}
		}
	}
	return ct.Build()
}

// reachInsert counts what the insert s of the call c, whose outcome in the engine is outcome,
// reaches on the database of o, as [reach] says.
func reachInsert(t testing.TB, o *oracle, s *stmt.Insert, c stmttest.Call, outcome string, count func(string)) {
	t.Helper()
	if s.Parameters() > 0 {
		for _, a := range c.Args {
			count("argument " + stmttest.ArgumentKind(a))
		}
	}
	args := oracleArgs(c.Args)
	refused := len(args) != s.Parameters() || slices.ContainsFunc(args, func(a any) bool { return refusal(a) != "" })
	// value of the term, a literal or its argument as SQLite binds it, or nil for an argument that the
	// engine refuses.
	value := func(term stmt.Term) any {
		switch {
		case term.Parameter == 0:
			return term.Value
		case refused:
			return nil
		}
		return args[term.Parameter-1]
	}
	long := false
	for _, term := range s.Values {
		if term.Parameter == 0 {
			count("literal " + stmttest.LiteralKind(term.Value))
		}
		long = long || byteLength(value(term)) > 900
	}
	if long {
		count("long value")
	}
	_, columns := o.columns(t, s.Table.Value)
	if columns == nil {
		return
	}
	if o.prepare(t, c.Src) == nil {
		typ := keyType(t, o, s)
		count("insert into " + typ + " " + outcome)
		if positions, ok := stmttest.Positions(columns, s); ok && outcome == "UNIQUE constraint failed" {
			for i, position := range positions {
				if !columns[position].PrimaryKey {
					continue
				}
				switch v := value(s.Values[i]).(type) {
				case float64:
					if v == 0 && math.Signbit(v) {
						count("-0.0 argument into " + typ + " " + outcome)
					}
				case stmt.Real:
					if v == 0 && math.Signbit(float64(v)) {
						count("-0.0 literal into " + typ + " " + outcome)
					}
				}
			}
		}
	}
	if !refused {
		for _, what := range stmttest.NullCases(columns, s, args) {
			count(what + " " + outcome)
		}
	}
	if len(columns) < 2 || refused {
		return
	}
	form := "insert without a list"
	if s.Columns != nil {
		form = "insert with a list in the order of the table"
		if positions, ok := stmttest.Positions(columns, s); ok && !slices.IsSorted(positions) {
			form = "insert with a list in another order"
		}
	}
	count(form + " " + outcome)
	switch faults := stmttest.Faults(columns, s, args); len(faults) {
	case 0:
	case 1:
		count("insert with " + faults[0] + ": " + outcome)
	default:
		count("insert with two faults: " + outcome)
	}
}

// checkArguments of the call c, whose answer of the engine is got, against s4ql's rule for the
// arguments of a text that holds a statement that the engine supports: another number of arguments
// than the statement has parameters is the error of [engine.ErrArguments], with both numbers, and
// otherwise an argument that no column holds, with the names dropped, is "not supported" for the
// first such argument, as [refusal] says. It gives the kind of the excuse, [excusedArguments] or
// [excusedArgument], if the rule decides the call, or "", and what differs from the rule, or "".
func checkArguments(c stmttest.Call, got answer) (kind, failure string) {
	s, err := stmt.Check(c.Src)
	if err != nil || s == nil {
		return "", ""
	}
	args := unnamed(c.Args)
	if n := s.Parameters(); len(args) != n {
		want := fmt.Sprintf("s4ql: wrong number of arguments: %v for a statement with %v parameter", len(args), n)
		if n != 1 {
			want += "s"
		}
		if got.err != wrongArguments || got.message != want {
			return excusedArguments, fmt.Sprintf("%q with %v arguments: the engine gives %v (%q), and the rule %q", c.Src, len(args), got, got.message, want)
		}
		return excusedArguments, ""
	}
	for j, a := range args {
		which := refusal(a)
		if which == "" {
			continue
		}
		want := fmt.Sprintf("s4ql: not supported: argument %v, which %v", j+1, which)
		if got.err != "not supported" || got.message != want {
			return excusedArgument, fmt.Sprintf("%q with the argument %#v: the engine gives %v (%q), and the rule %q", c.Src, a, got, got.message, want)
		}
		return excusedArgument, ""
	}
	return "", ""
}

// refusal of the argument a, as [values] gives it to the engine: how the engine's message says why
// no column holds it, or "" for a value that a column holds.
func refusal(a any) string {
	if _, ok := stmttest.Integers([]any{a}); ok {
		return ""
	}
	switch a := converted(a).(type) {
	case float64:
		switch {
		case math.IsNaN(a):
			return "is a real that is not a number"
		case math.IsInf(a, 0):
			return "is an infinite real"
		}
		return ""
	case string:
		switch {
		case !utf8.ValidString(a):
			return "is text that is not valid UTF-8"
		case strings.ContainsRune(a, 0):
			return "is text that holds the character NUL"
		}
		return ""
	case []byte:
		if a != nil {
			return ""
		}
	case nil:
		// NULL, which a column that takes NULL holds, and SQLite's NOT NULL refuses for any other.
		return ""
	}
	return "has no value"
}

// oracleArgs of the arguments args of a call, as SQLite binds them: each Go integer as an int64, a
// float32 as a float64, and every other argument as it is, with the names dropped.
func oracleArgs(args []any) []any {
	var as []any
	for _, a := range unnamed(args) {
		if v, ok := stmttest.Integers([]any{a}); ok {
			a = v[0]
		}
		as = append(as, converted(a))
	}
	return as
}

// checkType of the call c, with the arguments args as SQLite binds them, whose answer of the engine is
// got, against s4ql's rule for the type of a value, as [typeRule] gives it. It gives [excusedType] if
// the rule decides the call, or "", and what differs from the rule, or "". In a read-only
// transaction the rule decides nothing for an insert: SQLite and the engine refuse the write before
// they look at its value.
func (p *pair) checkType(c stmttest.Call, args []any, got answer) (excuse, failure string) {
	t := p.t
	t.Helper()
	if slices.ContainsFunc(args, func(a any) bool { return p.i.limit > 0 && byteLength(a) > p.i.limit }) || p.longCondition(c.Src) {
		// An argument that is too long gets SQLite's error when SQLite binds it, which the caller
		// compares, and so does a literal of a condition that is too long, which s4ql refuses before the
		// type of a value, and SQLite mostly too.
		return "", ""
	}
	want := typeRule(t, p.o, c.Src, args)
	if p.readOnly {
		want = readOnlyRule(t, p.o, c.Src, args)
	}
	if want == "" {
		return "", ""
	}
	if got.err != "not supported" || got.message != want {
		return excusedType, fmt.Sprintf("%q with the arguments %#v: the engine gives %v (%q), and the rule %q", c.Src, args, got, got.message, want)
	}
	return excusedType, ""
}

// checkLength of the call c, whose answer of the engine is got, against s4ql's rule for a literal
// that is longer than the length limit: an insert, an update, or a delete that SQLite prepares fails
// with SQLite's `string or blob too big`, after the names, for a literal of an insert, of SET, or of
// a condition of an update or a delete. SQLite meets such a literal only with a limit below the
// length of the text that holds it, and then gives the error for a string literal only when it steps
// the statement, which an update or a delete does only for a row that it finds, and turns a blob
// literal into NULL, so the call is not run in SQLite. It gives [excusedLong] for an insert and
// [excusedLongChange] for an update or a delete if the rule decides the call, or "", and what differs
// from the rule, or "". A select with such a literal in a condition is compared with SQLite, as
// [pair.longCondition] says.
func (p *pair) checkLength(c stmttest.Call, got answer) (excuse, failure string) {
	t := p.t
	t.Helper()
	s, _ := stmt.Check(c.Src)
	excuse = excusedLong
	var terms []stmt.Term
	switch s := s.(type) {
	case *stmt.Insert:
		terms = s.Values
	case *stmt.Update, *stmt.Delete:
		excuse = excusedLongChange
		terms = changeTerms(s)
	default:
		return "", ""
	}
	var n int
	for _, v := range terms {
		n = max(n, byteLength(v.Value))
	}
	if p.i.limit == 0 || n <= p.i.limit || p.o.prepare(t, c.Src) != nil {
		return "", ""
	}
	if got.err != tooBig {
		return excuse, fmt.Sprintf("%q with a literal of %v bytes: the engine gives %v, and the rule %q", c.Src, n, got, tooBig)
	}
	return excuse, ""
}

// recordSlips reports whether the update u, with the arguments args as SQLite binds them, which the
// engine refused with `string or blob too big` for the record of a row, and which SQLite ran without
// that error, succeeding or failing with `UNIQUE constraint failed`, is the known difference of a
// record that is too long in docs/todo.md: it finds two rows or more, and when the rows change one at
// a time in the order of the key, as the engine changes them, each by an update of that row alone
// in SQLite, in a savepoint that is rolled back, the first error is `string or blob too big` for the
// record of a row with the values of its SET. SQLite's `OP_MakeRecord` checks the
// limit only when the buffer of the record of an earlier row of the statement is too small for the
// next, and that buffer has the size that the allocator gives, so a statement that changes many rows
// can store a record over the limit by a few bytes, on some platforms. recordSlips leaves SQLite's
// count of changes at 0, as the engine's is after the refusal.
func (p *pair) recordSlips(u *stmt.Update, args []any) bool {
	t, o := p.t, p.o
	t.Helper()
	table, columns := o.columns(t, u.Table.Value)
	key := columns[slices.IndexFunc(columns, func(c stmttest.TableColumn) bool { return c.PrimaryKey })].Name
	found, foundArgs, one, oneArgs := stmttest.RowQueries(u, key, args)
	is.Equal(t, "", o.run(t, "savepoint s4ql_rows").err)
	is.Equal(t, "", o.run(t, found, foundArgs...).err, found)
	n, err := strconv.Atoi(o.run(t, "select count(*) from temp.s4ql_found").rows[0])
	is.NotError(t, err)
	// The rows change one at a time, in the order of the key, as the engine changes them, and the
	// first error is the one that the engine must give.
	first := ""
	for i := 0; i < n && first == ""; i++ {
		first = o.run(t, one, append(slices.Clone(oneArgs), int64(i))...).err
	}
	is.Equal(t, "", o.run(t, "rollback to s4ql_rows").err)
	is.Equal(t, "", o.run(t, "release s4ql_rows").err)
	is.Equal(t, "", o.run(t, "update "+stmttest.Quote(table)+" set "+stmttest.Quote(key)+" = "+stmttest.Quote(key)+" where 0").err)
	p.prev = 0
	return n >= 2 && first == tooBig
}

// changeTerms of the update or the delete s: the values of its SET, and of its conditions, in the
// order of the text.
func changeTerms(s stmt.Statement) []stmt.Term {
	var terms []stmt.Term
	var where []stmt.Condition
	switch s := s.(type) {
	case *stmt.Update:
		for _, a := range s.Set {
			terms = append(terms, a.Value)
		}
		where = s.Where
	case *stmt.Delete:
		where = s.Where
	}
	for _, c := range where {
		terms = append(terms, c.Value)
	}
	return terms
}

// longChange of src, if it holds an update or a delete with a literal longer than the length limit
// of the pair: "update" or "delete", and where the first such literal is, "SET" or "WHERE"; or "" and
// "".
func (p *pair) longChange(src string) (kind, in string) {
	s, _ := stmt.Check(src)
	if p.i.limit == 0 {
		return "", ""
	}
	switch s := s.(type) {
	case *stmt.Update:
		for _, a := range s.Set {
			if byteLength(a.Value.Value) > p.i.limit {
				return "update", "SET"
			}
		}
		if slices.ContainsFunc(s.Where, func(c stmt.Condition) bool { return byteLength(c.Value.Value) > p.i.limit }) {
			return "update", "WHERE"
		}
	case *stmt.Delete:
		if slices.ContainsFunc(s.Where, func(c stmt.Condition) bool { return byteLength(c.Value.Value) > p.i.limit }) {
			return "delete", "WHERE"
		}
	}
	return "", ""
}

// longCondition reports whether src holds a select with a literal in a condition that is longer than
// the length limit of the pair. By s4ql's rule such a select, whose names the table has and whose
// arguments s4ql takes, fails with SQLite's `string or blob too big` before the type of a value is
// checked, as an insert does. SQLite gives the same for a string literal, which it evaluates once
// before it reads a row, but for one on the upper bound of the key whose search reaches no row, and
// turns a blob literal into NULL, so that the select gives no rows: the known difference of
// docs/todo.md.
func (p *pair) longCondition(src string) bool {
	s, _ := stmt.Check(src)
	sel, ok := s.(*stmt.Select)
	return ok && p.i.limit > 0 && slices.ContainsFunc(sel.Where, func(c stmt.Condition) bool { return byteLength(c.Value.Value) > p.i.limit })
}

// tooBig is the err of an [answer] of SQLite's error of a text or a blob that is too long.
const tooBig = "18/18: string or blob too big"

// byteLength of a value, a literal or an argument, that is text or a blob, and 0 for any other.
func byteLength(v any) int {
	switch v := v.(type) {
	case stmt.Text:
		return len(v)
	case stmt.Blob:
		return len(v)
	case string:
		return len(v)
	case []byte:
		return len(v)
	}
	return 0
}

// typeRule of the insert, the select, the update, or the delete of the text src, with the arguments
// args as SQLite binds them, on the database of o: s4ql's "not supported" for the first column, in
// the order of the table, whose value, a literal or an argument, is of another type than the column,
// where SQLite would convert the value or give an error of its own, as [stmttest.TypeRule] gives it,
// and for the first condition of a select, in the order of the text, whose value is of another type
// than its column, as [stmttest.ConditionRule] gives it, and as [stmttest.UpdateRule] and
// [stmttest.DeleteRule] give it for an update and a delete; or "". It is "" for another text, a
// statement that SQLite does not prepare, which gets SQLite's error, one with another number of
// arguments than parameters, an insert that leaves a column without a value, which gets SQLite's
// `NOT NULL constraint failed` first, and a statement whose every value has the type of its column.
func typeRule(t testing.TB, o *oracle, src string, args []any) string {
	t.Helper()
	s, _ := stmt.Check(src)
	if s == nil || len(args) != s.Parameters() {
		return ""
	}
	switch s := s.(type) {
	case *stmt.Insert:
		if o.prepare(t, src) != nil {
			return ""
		}
		table, columns := o.columns(t, s.Table.Value)
		return stmttest.TypeRule(table, columns, s, args)
	case *stmt.Select:
		if s.Where == nil || o.prepare(t, src) != nil {
			return ""
		}
		table, columns := o.columns(t, s.Table.Value)
		return stmttest.ConditionRule(table, columns, s, args)
	case *stmt.Update:
		if o.prepare(t, src) != nil {
			return ""
		}
		table, columns := o.columns(t, s.Table.Value)
		return stmttest.UpdateRule(table, columns, s, args, func() bool {
			q, bound := stmttest.FoundQuery(s.Table, s.Where, args)
			return len(o.run(t, q, bound...).rows) > 0
		})
	case *stmt.Delete:
		if o.prepare(t, src) != nil {
			return ""
		}
		table, columns := o.columns(t, s.Table.Value)
		return stmttest.DeleteRule(table, columns, s, args)
	}
	return ""
}

// readOnlyRule is [typeRule] in a read-only transaction, which refuses an insert, and the SET of an
// update, before it looks at a value: the rule of the WHERE of an update, of a select, and of a
// delete, and "" for an insert.
func readOnlyRule(t testing.TB, o *oracle, src string, args []any) string {
	t.Helper()
	s, _ := stmt.Check(src)
	switch s := s.(type) {
	case *stmt.Insert:
		return ""
	case *stmt.Update:
		// SQLite prepares the update only if the table has every column that it names.
		if len(args) != s.Parameters() || o.prepare(t, src) != nil {
			return ""
		}
		table, columns := o.columns(t, s.Table.Value)
		return stmttest.WhereRule(table, columns, s.Where, args)
	}
	return typeRule(t, o, src, args)
}

// keyType is the declared type of the key column of the table of the insert, on the database of o,
// or "" if SQLite has no such table.
func keyType(t testing.TB, o *oracle, insert *stmt.Insert) string {
	t.Helper()
	_, columns := o.columns(t, insert.Table.Value)
	for _, c := range columns {
		if c.PrimaryKey {
			return c.Type
		}
	}
	return ""
}

// columns of the table with the name, as SQLite compares names, in SQLite, in the order of the
// table, and the name of the table as the table stores it. They are nil and "" if SQLite has no such
// table.
func (o *oracle) columns(t testing.TB, name string) (string, []stmttest.TableColumn) {
	t.Helper()
	s, err := o.c.Prepare(`select m.name, p.name, p.type, p.pk, p."notnull" from sqlite_schema m, pragma_table_info(m.name) p where m.type = 'table' and m.name = ?1 collate nocase order by p.cid`)
	is.NotError(t, err)
	defer func() { is.NotError(t, s.Finalize()) }()
	is.NotError(t, s.BindText(1, name))
	var table string
	var columns []stmttest.TableColumn
	for {
		ok, err := s.Step()
		is.NotError(t, err)
		if !ok {
			return table, columns
		}
		table = s.ColumnText(0)
		// The key of a table WITHOUT ROWID never takes NULL, also when SQLite reports it without NOT
		// NULL.
		key := s.ColumnInt64(3) == 1
		columns = append(columns, stmttest.TableColumn{Name: s.ColumnText(1), Type: s.ColumnText(2), PrimaryKey: key, Nullable: !key && s.ColumnInt64(4) == 0})
	}
}

// typeRefusal reports whether message is the engine's refusal of a value of another type than its
// column, for the statement of the text src, as [typeRule] gives it.
func typeRefusal(src, message string) bool {
	s, _ := stmt.Check(src)
	switch s := s.(type) {
	case *stmt.Insert:
		return strings.Contains(message, " for the column ")
	case *stmt.Select:
		return s.Where != nil && strings.Contains(message, " compared with the column ")
	case *stmt.Update:
		return strings.Contains(message, " for the column ") || strings.Contains(message, " compared with the column ")
	case *stmt.Delete:
		return strings.Contains(message, " compared with the column ")
	}
	return false
}

// changesRows reports whether src is a supported insert, update, or delete, which sets SQLite's count
// of changes.
func changesRows(src string) bool {
	s, _ := stmt.Check(src)
	switch s.(type) {
	case *stmt.Insert, *stmt.Update, *stmt.Delete:
		return true
	}
	return false
}

// syntaxOfNotSupported gives the message of the syntax error that SQLite gives when it prepares src, a
// text that the engine does not support, on the empty database empty, or "". A text that the engine
// does not support is one that SQLite accepts, so the message must be "".
func syntaxOfNotSupported(t testing.TB, empty *oracle, src string) string {
	t.Helper()
	// A text with a NUL byte or a byte that is not UTF-8 is not supported, whatever else it holds.
	if !utf8.ValidString(src) || strings.ContainsRune(src, 0) {
		return ""
	}
	if err := empty.prepare(t, src); err != nil && sqloracle.IsSyntaxMessage(err.Message) {
		return err.Message
	}
	return ""
}

// TestDifferential runs random sequences of statements on the engine and on SQLite, and requires
// the same answers, the same count of changes, and the same tables and rows, half of them with
// restarts of the instance, with an empty cache and with its own, and half of them with the length
// limit lowered to [stmttest.LengthLimit].
func TestDifferential(t *testing.T) {
	n := *sequences
	if slow() {
		n /= 10
	}
	y := tally{}
	for s := range uint64(n) {
		differential(t, *seed+s, *length, s%2 == 1, map[bool]int{true: stmttest.LengthLimit}[s%4 >= 2], y)
	}
	t.Logf("%v sequences of %v statements: %v", n, *length, y)
	if !slow() && atDefault("seed", "sequences", "length") {
		// What the full run must reach, at least: each type as a key, with an insert that succeeds,
		// meets a key that exists, and has a value of another type, also -0.0 after 0.0; each kind of
		// literal and of argument, also those that no column holds; and a text or a blob that is too
		// long, as a literal and as an argument; and the forms and the faults of slices 5 and 6. Each
		// least number is about a third of what the run gave on 2026-11-20, so that a generator that
		// stops reaching a case fails the test.
		for kind, least := range reachedDifferential {
			if y["reached: "+kind] < least {
				t.Errorf("the run reaches %q %v times, and at least %v were expected: the generator stopped reaching it", kind, y["reached: "+kind], least)
			}
		}
		if y["excused: "+excusedLong] == 0 {
			t.Errorf("no literal is longer than the length limit")
		}
	}
	// The outcomes of a parameter that the run must reach: an insert through a parameter that
	// succeeds, that meets a key that exists, and that the rule for arguments refuses, and an argument
	// for a statement without a parameter.
	for _, kind := range []string{
		"insert with a parameter ok", "insert with a parameter UNIQUE constraint failed", "insert with a parameter wrong number of arguments",
		"insert with a parameter not supported", "insert wrong number of arguments", "select wrong number of arguments",
		// The errors of the names of slice 5.
		"createtable duplicate column name", "select no such column", "insert NOT NULL constraint failed", "insert values were supplied", "insert  values for ",
	} {
		// The tenth of the sequences of a slow run, under the race detector or the address sanitizer, has
		// no create table with a column named twice since the sequence writes updates and deletes; the
		// full run has 15.
		if y[kind] == 0 && (kind != "createtable duplicate column name" || !slow()) {
			t.Errorf("no statement has the outcome %q", kind)
		}
	}
	// The texts that the full run excuses from the comparison with SQLite, by kind. Every other kind,
	// such as each known gap, is pinned at 0. Of the calls that are excused because s4ql refuses
	// their arguments by its rule, each is checked against the rule instead.
	if !slow() && atDefault("seed", "sequences", "length") {
		// Before the sequence wrote tables of several columns, the counts were 21, 644, 140, 371, and
		// 5. Its inserts have up to four values, and so more parameters and more arguments around
		// them, which took the second and the third up; one insert in ten has a value of any type,
		// where one value in ten had, which took the fourth down; and the sequence draws other random
		// numbers, which moved the first and the last. The engine of slice 6 took the third from 197 to
		// 184: a nil argument is NULL, which the engine takes, and the 13 calls with one run in SQLite
		// now. The generator drew as before then. When the sequence wrote NULL, they went from 17, 718,
		// 184, 336, and 3 to 18, 703, 220, 339, and 8: the sequence draws other random numbers, which
		// moves each of them, and of its own a call around a NULL argument has a nil []byte in its
		// place, which took the third up: the arguments that are a nil []byte went from 26 to 54. Nil is
		// no longer among [stmttest.Unsupported]. The engine of slice 7 took the fourth from 339 to 350:
		// it runs the two selects with WHERE of [stmttest.Others], which compare a column with a small
		// integer or with an argument of any type, and 11 of them compare one of another type than its
		// column, which s4ql's rule refuses. The generator drew as before then. When the sequence wrote
		// WHERE, they went from 18, 703, 220, 350, and 8 to 19, 718, 248, 478, and 4: the sequence
		// draws other random numbers, which moves each of them, and of its own one select in two has a
		// WHERE, which compares a value of another type than its column in 112 selects of the sequence,
		// which took the fourth up, and has parameters, whose arguments [stmttest.Sequence.Next] makes
		// wrong in number or of no column now and then, which took the second and the third up. The
		// selects with a literal in a condition longer than the length limit, of which the run has 2,
		// give `string or blob too big` in SQLite too, so [excusedLongCondition] is 0. The engine of
		// slice 8 took the fourth from 478 to 483: it runs the update of [stmttest.Others], which sets
		// a column to a small integer, and 5 of them give it to a column of another type, which s4ql's
		// rule refuses. The generator drew as before then. When the sequence wrote updates and deletes,
		// they went from 19, 718, 248, 483, and 4 to 14, 707, 300, 576, and 2, and [excusedLongChange]
		// is 4: the sequence draws other random numbers, which moves each of them, and writes fewer
		// inserts, which took the last down; of its own an update or a delete has parameters, whose
		// arguments [stmttest.Sequence.Next] makes wrong in number or of no column now and then, which
		// took the third up, and an update gives a column a value of another type, or compares one in
		// its WHERE, now and then, which took the fourth up. The 4 are updates and deletes with a literal
		// longer than the length limit, in SET or in WHERE.
		want := map[string]int{acceptedStartedTable: 14, excusedArguments: 707, excusedArgument: 300, excusedType: 576, excusedLong: 2, excusedLongChange: 4}
		kinds := slices.Collect(maps.Keys(want))
		for name := range y {
			// The count of [excusedRecordSlip] depends on the allocator of the platform.
			if kind, ok := strings.CutPrefix(name, "excused: "); ok && !slices.Contains(kinds, kind) && kind != excusedRecordSlip {
				kinds = append(kinds, kind)
			}
		}
		slices.Sort(kinds)
		for _, kind := range kinds {
			pinned(t, "texts excused from the comparison with SQLite as "+kind, want[kind], y["excused: "+kind])
		}
	}
}

// reachedDifferential is the least number of each kind of what [pair.reach] counts that the full run
// of [TestDifferential] must reach.
var reachedDifferential = map[string]int{
	"insert into INT ok": 130, "insert into INTEGER ok": 70, "insert into REAL ok": 100, "insert into TEXT ok": 110, "insert into BLOB ok": 100,
	"insert into INT UNIQUE constraint failed": 50, "insert into INTEGER UNIQUE constraint failed": 26, "insert into REAL UNIQUE constraint failed": 34,
	"insert into TEXT UNIQUE constraint failed": 26, "insert into BLOB UNIQUE constraint failed": 42,
	"-0.0 literal into REAL UNIQUE constraint failed": 2, "-0.0 argument into REAL UNIQUE constraint failed": 2,
	"insert into INT a value of another type": 20, "insert into INTEGER a value of another type": 20, "insert into REAL a value of another type": 24,
	"insert into TEXT a value of another type": 24, "insert into BLOB a value of another type": 24,
	"insert into TEXT string or blob too big": 7, "insert into BLOB string or blob too big": 5, "long value": 90,
	"literal integer": 950, "literal real": 390, "literal real -0.0": 68, "literal text": 400, "literal empty text": 76, "literal blob": 390, "literal empty blob": 100,
	"argument int64": 440, "argument int": 7, "argument int32": 4, "argument uint64": 7, "argument float64": 200, "argument float64 -0.0": 26,
	"argument float32": 9, "argument string": 170, "argument empty string": 34, "argument []uint8": 170, "argument empty []byte": 45,
	"argument NaN": 4, "argument infinite float64": 7, "argument float32 that is not finite": 5, "argument string that is not valid UTF-8": 12,
	"argument string with NUL": 14, "argument nil": 88, "argument nil []byte": 18, "argument bool": 3, "argument time.Time": 4,
	"argument uint64 above int64": 4, "argument named": 40,

	// Slice 5: tables of one to three columns, of mixed types and with two columns of a type, with
	// the key at each position; inserts without a list and with a list in the order of the table and
	// in another, and with each fault and two; selects of *, of names in another order, of a name
	// twice, and of a name that the table does not have, also in double quotes.
	"create table of 1 columns ok": 70, "create table of 2 columns ok": 90, "create table of 3 columns ok": 70,
	"create table of mixed types ok": 76, "create table with two columns of a type ok": 120,
	"create table with the key at 0 ok": 140, "create table with the key at 1 ok": 68, "create table with the key at 2 ok": 26,
	"insert without a list ok": 130, "insert with a list in the order of the table ok": 120, "insert with a list in another order ok": 86,
	"insert with a list in another order UNIQUE constraint failed": 26, "insert with a list in another order a value of another type": 17,
	"insert with a list in another order NOT NULL constraint failed":         8,
	"insert with a column that the table does not have: has no column named": 29, "insert with a column without a value: NOT NULL constraint failed": 43,
	"insert with the wrong number of values: values were supplied": 37, "insert with the wrong number of values:  values for ": 44,
	"insert with a value of another type in the key: a value of another type": 31, "insert with a value of another type in another column: a value of another type": 33,
	"insert with two faults: NOT NULL constraint failed": 8, "insert with two faults: a value of another type": 8, "insert with two faults: has no column named": 5,
	"select * of 1 columns ok": 86, "select * of 2 columns ok": 100, "select * of 3 columns ok": 90, "select of names ok": 250,
	"select of names in another order ok": 55, "select of a name twice ok": 170,
	"select of a name that the table does not have no such column": 17, "select of a name in double quotes that the table does not have no such column": 24,

	// Slice 6, a third of what the run gave on 2026-11-20 when the sequence wrote NULL: tables of
	// which no column, every column, and some columns outside the key take NULL; NULL as a literal
	// and as an argument in a column that takes it, also beside a key that exists and a value of
	// another type, and in the key and in a column that does not take it; a column that takes NULL
	// left out of a list; NULL beside a value of another type in another column, which gets NOT NULL
	// or the refusal of the type by where the NULL is; and a select of * and of names with NULL among
	// its values.
	"create table of which no column takes NULL ok": 70, "create table of which every column outside the key takes NULL ok": 70,
	"create table of which some columns outside the key take NULL ok": 16, "literal null": 220,
	"NULL literal into a column that takes NULL ok": 34, "NULL argument into a column that takes NULL ok": 16,
	"NULL literal into a column that takes NULL UNIQUE constraint failed": 11, "NULL argument into a column that takes NULL UNIQUE constraint failed": 5,
	"NULL literal into a column that takes NULL a value of another type": 5, "NULL argument into a column that takes NULL a value of another type": 2,
	"NULL literal into the key NOT NULL constraint failed": 35, "NULL argument into the key NOT NULL constraint failed": 14,
	"NULL literal into a column that does not take NULL NOT NULL constraint failed": 10, "NULL argument into a column that does not take NULL NOT NULL constraint failed": 4,
	"insert that leaves out a column that takes NULL ok": 60, "insert that leaves out a column that takes NULL UNIQUE constraint failed": 16,
	"insert that leaves out a column that takes NULL a value of another type": 11, "insert that leaves out a column that takes NULL NOT NULL constraint failed": 13,
	"insert with NULL in the key: NOT NULL constraint failed": 17, "insert with NULL in a column that does not take it: NOT NULL constraint failed": 11,
	"insert of NULL beside a value of another type NOT NULL constraint failed": 4, "insert of NULL beside a value of another type a value of another type": 6,
	"select * with NULL ok": 45, "select of names with NULL ok": 7,

	// Slice 7, a third of what the run gave on 2026-11-20 when the sequence wrote WHERE: each operator
	// on the key and on another column, and with a value of another type on the key; each way that the
	// plan reads rows, also two bounds that hold no key; a select that gives no row, some rows, and
	// every row of a table with rows; each kind of value, by literal and by argument, NULL by both;
	// and the faults: a column that the table does not have, in a comparison and under IS NULL or IS
	// NOT NULL, alone, before, and after other conditions, and in double quotes; a value of another
	// type; an argument that no column holds; the wrong number of arguments; a column tested twice;
	// contradicting conditions; and a literal longer than the length limit. Every select with WHERE is
	// read both ways.
	"= on the key ok": 22, "!= on the key ok": 27, "< on the key ok": 47, "<= on the key ok": 39, "> on the key ok": 39, ">= on the key ok": 42,
	"IS NULL on the key ok": 19, "IS NOT NULL on the key ok": 16,
	"= on another column ok": 10, "!= on another column ok": 8, "< on another column ok": 9, "<= on another column ok": 6, "> on another column ok": 11,
	">= on another column ok": 7, "IS NULL on another column ok": 2, "IS NOT NULL on another column ok": 8,
	"= on the key a value of another type": 11, "!= on the key a value of another type": 11, "< on the key a value of another type": 14,
	"<= on the key a value of another type": 11, "> on the key a value of another type": 12, ">= on the key a value of another type": 11,
	"WHERE that reads no row ok": 29, "WHERE that reads a get of one key ok": 18, "WHERE that reads a scan from a lower bound ok": 15,
	"WHERE that reads a scan to an upper bound ok": 16, "WHERE that reads a scan between two bounds ok": 19,
	"WHERE that reads a scan between two bounds that hold no key ok": 18, "WHERE that reads a scan of every row ok": 38,
	"WHERE that gives no row of a table with rows": 58, "WHERE that gives some rows of a table": 20, "WHERE that gives every row of a table with rows": 13,
	"comparison with literal integer ok": 79, "comparison with literal real ok": 28, "comparison with literal real -0.0 ok": 2, "comparison with literal text ok": 28,
	"comparison with literal empty text ok": 7, "comparison with literal blob ok": 28, "comparison with literal empty blob ok": 6, "comparison with literal null ok": 7,
	"comparison with argument int64 ok": 34, "comparison with argument float64 ok": 9, "comparison with argument float64 -0.0 ok": 2,
	"comparison with argument string ok": 11, "comparison with argument empty string ok": 3, "comparison with argument []uint8 ok": 11,
	"comparison with argument empty []byte ok": 6, "comparison with argument nil ok": 5,
	"WHERE with a column that the table does not have in a comparison, alone no such column":                                13,
	"WHERE with a column that the table does not have in a comparison, before other conditions no such column":              26,
	"WHERE with a column that the table does not have in a comparison, after other conditions no such column":               14,
	"WHERE with a column that the table does not have under IS NULL or IS NOT NULL, before other conditions no such column": 4,
	"WHERE with a column that the table does not have under IS NULL or IS NOT NULL, after other conditions no such column":  3,
	"WHERE with a column in double quotes that the table does not have no such column":                                      32,
	"WHERE with a value of another type a value of another type":                                                            37, "WHERE with an argument that no column holds not supported": 12,
	"WHERE with the wrong number of arguments wrong number of arguments": 19, "WHERE that tests a column more than once ok": 87,
	"WHERE with contradicting conditions ok": 21, "WHERE with a literal longer than the length limit": 1, "read both ways": 444,

	// Slice 8, a third of what the run gave on 2026-11-20 when the sequence wrote updates and deletes:
	// each with and without WHERE, that changes rows and that finds none, of a table with rows that it
	// changes every row, some rows, and none of; a change of the key that succeeds, meets a key that
	// exists, and gets NULL; NULL for a column that does not take it, with a row found and without; a
	// column of SET that the table does not have, also in double quotes; a value of another type in SET
	// and in WHERE; each kind of value of SET, by literal and by argument; a column of WHERE that the
	// table does not have; NOT NULL and UNIQUE with and without WHERE; a record that is too long; a
	// literal of SET longer than the length limit; arguments that s4ql refuses; each way that the scan
	// reads the rows; and every update and delete with WHERE changed both ways. The sequence writes
	// fewer inserts and selects since then, so the least numbers of "-0.0 literal into REAL UNIQUE
	// constraint failed" and "select of names with NULL ok" are a third of what the run gives now, 2
	// and 7, where they were 6 and 25; and no select of the run has a column that the table does not
	// have under IS NULL or IS NOT NULL alone, so the run requires it of a delete, whose WHERE is read
	// by the same conditions.
	"update with WHERE ok, rows changed": 17, "update with WHERE ok, no row found": 76, "update without WHERE ok, rows changed": 20, "update without WHERE ok, no row found": 17,
	"delete with WHERE ok, rows changed": 32, "delete with WHERE ok, no row found": 77, "delete without WHERE ok, rows changed": 23, "delete without WHERE ok, no row found": 12,
	"update that changes every row of a table with rows": 27, "update that changes some rows of a table": 10, "update that changes no row of a table with rows": 42,
	"delete that changes every row of a table with rows": 35, "delete that changes some rows of a table": 20, "delete that changes no row of a table with rows": 42,
	"SET of the key ok, rows changed": 19, "SET of the key UNIQUE constraint failed": 18, "SET of the key NOT NULL constraint failed": 9, "SET of every column ok, rows changed": 13,
	"SET of NULL for a column that does not take it NOT NULL constraint failed": 10, "SET of NULL for a column that does not take it ok, no row found": 13,
	"SET of a column that the table does not have no such column": 30, "SET of a column in double quotes that the table does not have no such column": 12,
	"SET with a value of another type a value of another type": 29, "SET with literal integer ok, rows changed": 15, "SET with literal real ok, rows changed": 5,
	"SET with literal real -0.0 ok, rows changed": 1, "SET with literal text ok, rows changed": 5, "SET with literal empty text ok, rows changed": 1,
	"SET with literal blob ok, rows changed": 4, "SET with literal empty blob ok, rows changed": 1, "SET with literal null ok, rows changed": 2,
	"SET with argument int64 ok, rows changed": 4, "SET with argument float64 ok, rows changed": 2, "SET with argument string ok, rows changed": 1,
	"SET with argument []uint8 ok, no row found": 5, "SET with argument nil ok, rows changed": 1,
	"update with WHERE no such column": 34, "update WHERE with a column in double quotes that the table does not have no such column": 11,
	"update WHERE with a value of another type a value of another type": 24, "update with WHERE NOT NULL constraint failed": 4, "update without WHERE NOT NULL constraint failed": 5,
	"update with WHERE UNIQUE constraint failed": 6, "update without WHERE UNIQUE constraint failed": 12, "update of a row whose record is too long": 1,
	"update with arguments that s4ql refuses not supported": 10, "update with arguments that s4ql refuses wrong number of arguments": 22,
	"update with a literal longer than the length limit in SET": 1,
	"update WHERE that reads no row ok, no row found":           13, "update WHERE that reads a get of one key ok, rows changed": 10, "update WHERE that reads a scan from a lower bound ok, rows changed": 1,
	"update WHERE that reads a scan to an upper bound ok, rows changed": 1, "update WHERE that reads a scan between two bounds ok, rows changed": 1,
	"update WHERE that reads a scan between two bounds that hold no key ok, no row found": 5, "update WHERE that reads a scan of every row ok, rows changed": 3,
	"update WHERE that tests a column more than once ok, rows changed": 3, "update WHERE with contradicting conditions ok, no row found": 11,
	"delete with WHERE no such column": 18, "delete WHERE with a column in double quotes that the table does not have no such column": 13,
	"delete WHERE with a value of another type a value of another type": 18, "delete with arguments that s4ql refuses not supported": 7,
	"delete with arguments that s4ql refuses wrong number of arguments": 11, "delete WHERE that reads no row ok, no row found": 12,
	"delete WHERE that reads a get of one key ok, rows changed": 15, "delete WHERE that reads a scan from a lower bound ok, rows changed": 3,
	"delete WHERE that reads a scan to an upper bound ok, rows changed": 2, "delete WHERE that reads a scan between two bounds ok, rows changed": 4,
	"delete WHERE that reads a scan between two bounds that hold no key ok, no row found": 9, "delete WHERE that reads a scan of every row ok, rows changed": 7,
	"delete WHERE that tests a column more than once ok, rows changed": 7, "delete WHERE with contradicting conditions ok, no row found": 12,
	"delete WHERE with a column that the table does not have under IS NULL or IS NOT NULL, alone no such column": 1, "changed both ways": 576,
}

// TestPair_recordSlips pins the known difference of a record that is too long, of docs/todo.md, at the
// length limit of 1000: the engine refuses the update of two rows whose second record is too long,
// which SQLite stores on some platforms and refuses on others, and the comparison accepts either
// only by an update of each row alone in SQLite.
func TestPair_recordSlips(t *testing.T) {
	setup := []string{
		"create table t (k int primary key, a text, b text not null) strict, without rowid",
		"insert into t values (1, '', '')", "insert into t values (2, '" + strings.Repeat("x", 301) + "', '')",
	}
	cases := []struct {
		name, src string
		// tooBig reports whether the engine refuses the update for a record that is too long, and slips
		// whether [pair.recordSlips] reports the known difference.
		tooBig, slips bool
	}{
		{"should accept an update of two rows whose second record is too long", "update t set b = '" + strings.Repeat("x", 700) + "'", true, true},
		{"should accept such an update whose new key exists too", "update t set k = 1, b = '" + strings.Repeat("x", 700) + "'", true, true},
		{"should not accept an update of one row whose record is too long", "update t set b = '" + strings.Repeat("x", 700) + "' where k = 2", true, false},
		{"should not accept an update of two rows whose records are within the limit", "update t set b = '" + strings.Repeat("x", 600) + "'", false, false},
		{"should not accept an update whose first row meets a key that exists before the record of the second is too long", "update t set k = 2, b = '" + strings.Repeat("x", 700) + "'", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			y := tally{}
			p := newPair(t, newInstanceLimited(t, engine.Options{}, nil, 1000), y)
			for _, src := range setup {
				is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
			}
			got := p.i.execValues(c.src, nil)
			is.Equal(t, c.tooBig, got.err == tooBig, got.String())
			s, _ := stmt.Check(c.src)
			is.Equal(t, c.slips, p.recordSlips(s.(*stmt.Update), nil))
			changes, _ := p.o.changes(t)
			is.Equal(t, int64(0), changes, "SQLite's count of changes after the check")
			if c.tooBig {
				is.Equal(t, p.o.state(t), p.i.state(), "SQLite's state after the check, which the engine's refusal did not change")
			}
		})
	}
}

// FuzzDifferential runs the sequence of statements that its input chooses, the seed and the number of
// statements, as [TestDifferential] does, on an engine on a database in memory, a test double of the
// core. Its input is a seed of the generator, not SQL text, so the guidance of the fuzzer by coverage
// has little to work with: it explores seeds of the generator, and its executions are not a measure
// of the SQL text that it covers. [FuzzSQL] gives the engine any text.
func FuzzDifferential(f *testing.F) {
	f.Add(uint64(0), uint8(40))
	f.Add(uint64(1), uint8(10))
	f.Fuzz(func(t *testing.T, seed uint64, n uint8) {
		differentialOn(t, seed, int(n%64), false, nil, newMemoryInstance(t))
	})
}

// FuzzDifferential_core runs the sequence of statements that its input chooses, the seed, the number
// of statements, whether the instance restarts, and whether the cache and SQLite have the lowered
// length limit [stmttest.LengthLimit], as [TestDifferential] does.
func FuzzDifferential_core(f *testing.F) {
	f.Add(uint64(0), uint8(40), false, false)
	f.Add(uint64(1), uint8(40), true, false)
	f.Add(uint64(2), uint8(40), false, true)
	f.Add(uint64(3), uint8(40), true, true)
	f.Fuzz(func(t *testing.T, seed uint64, n uint8, restarts, limited bool) {
		differential(t, seed, int(n%64), restarts, map[bool]int{true: stmttest.LengthLimit}[limited], nil)
	})
}

// fuzzSetUp is the database that [FuzzSQL] runs each text on: tables with names in different cases
// and in quotes, some with rows.
var fuzzSetUp = []string{
	"create table t1 (v1 int primary key) strict, without rowid",
	"insert into t1 (v1) values (1)",
	"insert into T1 (V1) values (-9223372036854775808)",
	`create table "Foo bar" ("Bar""x" int primary key) strict, without rowid`,
	`insert into "foo BAR" ("bar""X") values (7)`,
	"create table Été (abort int primary key) strict, without rowid",
	`create table "T""2" (v int primary key) strict, without rowid`,
}

// FuzzSQL runs any SQL text on the engine on a database in memory, a test double of the core, with
// the tables of [fuzzSetUp], and on SQLite with the same tables, and compares what each gives, as
// [TestDifferential] does, and the tables after it. It runs the text through Query, or through Exec
// if exec. [FuzzSQL_core] runs the same on the storage core, which is several tens of times slower.
func FuzzSQL(f *testing.F) {
	fuzzSQL(f, newMemoryInstance)
}

// FuzzSQL_core is [FuzzSQL] on the storage core, with a store in memory.
func FuzzSQL_core(f *testing.F) {
	fuzzSQL(f, func(t testing.TB) *instance { return newInstance(t, engine.Options{}) })
}

// fuzzSQL is [FuzzSQL] on the instances of newInstance. Its seeds are statements of the sequence of
// [TestDifferential], the texts of [stmttest.Others] and [fuzzSetUp], and every 500th text of the corpus.
func fuzzSQL(f *testing.F, newInstance func(t testing.TB) *instance) {
	q := newSequence(f, rand.New(rand.NewPCG(1, 2)))
	for range 40 {
		c := q.Next()
		f.Add(c.Src, !c.Query)
	}
	for _, src := range slices.Concat(fuzzSetUp, stmttest.Others) {
		if !strings.Contains(src, "%") {
			f.Add(src, false)
			continue
		}
		f.Add(fmt.Sprintf(src, `"foo BAR"`, `"BAR""x"`, 7), false)
		f.Add(fmt.Sprintf(src, "T1", "v1", 1), true)
	}
	for i, e := range corpus.All() {
		if i%500 == 0 {
			f.Add(e.SQL, i%1000 == 0)
		}
	}
	f.Fuzz(func(t *testing.T, src string, exec bool) {
		i := newInstance(t)
		p := newPair(t, i, nil)
		for _, s := range fuzzSetUp {
			if failure := p.step(stmttest.Call{Src: s, Supported: true}); failure != "" {
				t.Fatalf("the set-up: %v", failure)
			}
		}
		if failure := p.step(stmttest.Call{Src: src, Query: !exec}); failure != "" {
			t.Fatal(failure)
		}
		if got, want := i.state(), p.o.state(t); got != want {
			t.Fatalf("%q: the engine holds\n%v\nand SQLite\n%v", src, got, want)
		}
	})
}

// typesSetUp is the database that [FuzzTypes] runs on, with names in other cases than the inserts
// use: for each of the five types a table whose one column, the key, is of the type, a table whose
// first column is of the type and whose second, the key, is an INT, and a table of the same two
// columns whose first takes NULL.
var typesSetUp = []string{
	"create table T_int (v int primary key) strict, without rowid",
	"create table t_INTEGER (v integer primary key) strict, without rowid",
	"create table t_real (V real primary key) strict, without rowid",
	"create table t_text (v text primary key) without rowid, strict",
	"create table t_blob (v BLOB primary key) strict, without rowid",
	"create table T_int_k (v int not null, k int primary key) strict, without rowid",
	"create table t_INTEGER_k (v integer not null, K int primary key) strict, without rowid",
	"create table t_real_k (V real not null, k int primary key) strict, without rowid",
	"create table t_text_k (v text not null, k int primary key) without rowid, strict",
	"create table t_blob_k (v BLOB not null, k int primary key) strict, without rowid",
	"create table T_int_n (v int, k int primary key) strict, without rowid",
	"create table t_INTEGER_n (v integer, K int primary key) strict, without rowid",
	"create table t_real_n (V real, k int primary key) strict, without rowid",
	"create table t_text_n (v text, k int primary key) without rowid, strict",
	"create table t_blob_n (v BLOB, k int primary key) strict, without rowid",
}

// typesTables are the names of the tables of [typesSetUp], as the inserts of [FuzzTypes] name them.
// A name that ends in _k or _n is of a table of two columns, and one that ends in _n of a table whose
// first column takes NULL.
var typesTables = []string{
	"t_int", "t_integer", "t_real", "t_text", "t_blob", "t_int_k", "t_integer_k", "t_real_k", "t_text_k", "t_blob_k",
	"t_int_n", "t_integer_n", "t_real_n", "t_text_n", "t_blob_n",
}

// FuzzTypes gives the engine, on a database in memory, the bytes of its input as the value of an
// insert into a table of each of the five types, in the form that its input chooses: as a text
// argument, a blob argument, a string literal, a blob literal, the bits of a real argument or of an
// integer argument, or SQL text in the place of the value; or, in place of the bytes, NULL, by the
// length of the bytes as the literal, as a nil argument, or as the first column left out of the list
// of a table of two. The value is the key of a table of one column, and the first column of a table
// of two, which takes NULL in one of them, whose key the insert sets to 1 and names first in its
// list. Each insert runs twice, so that the second meets the key of the first, and a query reads
// the table. It compares each answer with SQLite's,
// as [TestDifferential] does, by s4ql's rules where they differ from SQLite: a value of another type
// than its column, NaN, an infinity, and text that holds NUL or is not valid UTF-8 are refused, and
// NULL is refused by SQLite's NOT NULL in the key and in a column that does not take it. Then
// it compares the tables. [FuzzTypes_core] runs the same on the storage core. The database in memory
// has no length limit, so the input of the limit that [FuzzTypes_core] reads changes nothing here.
func FuzzTypes(f *testing.F) {
	fuzzTypes(f, func(t testing.TB, limit int) *instance { return newMemoryInstance(t) })
}

// FuzzTypes_core is [FuzzTypes] on the storage core, with a store in memory and the length limit
// that its input chooses, from 1001 to 1255 bytes or SQLite's default, in the cache and in SQLite.
// After the inserts, it replays the log into an empty cache and compares the tables again.
func FuzzTypes_core(f *testing.F) {
	fuzzTypes(f, func(t testing.TB, limit int) *instance { return newInstanceLimited(t, engine.Options{}, nil, limit) })
}

// Forms of the value of [FuzzTypes].
const (
	formTextArgument = iota
	formBlobArgument
	formStringLiteral
	formBlobLiteral
	formRealArgument
	formIntegerArgument
	formNull
	formSQL
	forms
)

// typesCall is the insert of the bytes data into the table in the form, which [FuzzTypes] runs.
// The form is form % [forms], and an odd form / [forms] writes the hex digits of a blob literal and its
// x in upper case. The form [formNull] gives NULL by the length of data: as a nil argument for a
// length of 1 modulo 3, by leaving the first column out of the list of a table of two for 2, and
// otherwise as the literal.
func typesCall(table string, data []byte, form uint8) stmttest.Call {
	insert, end := "insert into "+table+" (v) values (", ")"
	two := strings.HasSuffix(table, "_k") || strings.HasSuffix(table, "_n")
	if two {
		insert = "insert into " + table + " (k, v) values (1, "
	}
	bits := func() uint64 {
		var b [8]byte
		copy(b[:], data)
		return binary.LittleEndian.Uint64(b[:])
	}
	switch form % forms {
	case formTextArgument:
		return stmttest.Call{Src: insert + "?" + end, Args: []any{string(data)}, Supported: true}
	case formBlobArgument:
		return stmttest.Call{Src: insert + "?" + end, Args: []any{append([]byte{}, data...)}, Supported: true}
	case formStringLiteral:
		text := string(data)
		// The pass supports the text of a statement only if it is valid UTF-8 without NUL.
		supported := utf8.ValidString(text) && !strings.ContainsRune(text, 0)
		return stmttest.Call{Src: insert + "'" + strings.ReplaceAll(text, "'", "''") + "'" + end, Supported: supported}
	case formBlobLiteral:
		x := "x'" + hex.EncodeToString(data) + "'"
		if form/forms%2 == 1 {
			x = strings.ToUpper(x)
		}
		return stmttest.Call{Src: insert + x + end, Supported: true}
	case formRealArgument:
		return stmttest.Call{Src: insert + "?" + end, Args: []any{math.Float64frombits(bits())}, Supported: true}
	case formIntegerArgument:
		return stmttest.Call{Src: insert + "?" + end, Args: []any{int64(bits())}, Supported: true}
	case formNull:
		switch {
		case len(data)%3 == 1:
			return stmttest.Call{Src: insert + "?" + end, Args: []any{nil}, Supported: true}
		case len(data)%3 == 2 && two:
			return stmttest.Call{Src: "insert into " + table + " (k) values (1)", Supported: true}
		}
		return stmttest.Call{Src: insert + "null" + end, Supported: true}
	default:
		return stmttest.Call{Src: insert + string(data) + end}
	}
}

// fuzzTypes is [FuzzTypes] on the instances of newInstance, which takes the length limit, 0 for
// SQLite's default. Its seeds are values at the edges of each type in every form.
func fuzzTypes(f *testing.F, newInstance func(t testing.TB, limit int) *instance) {
	bits := func(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }
	seeds := [][]byte{
		nil, []byte("a"), []byte("'"), []byte("it''s"), []byte("\x00"), []byte("a\x00b"), []byte("\xff"), []byte("\xc3"),
		[]byte("é"), []byte("💾"), []byte("1"), []byte("-0.0"), []byte("1e999"), []byte("0x10"), []byte("null"), []byte("x'0g'"),
		[]byte("-9223372036854775808"), []byte("9223372036854775808"), []byte("'a' || 'b'"), []byte(strings.Repeat("abc", 40)),
		bits(math.Float64bits(math.NaN())), bits(math.Float64bits(math.Inf(1))), bits(math.Float64bits(math.Copysign(0, -1))),
		bits(math.Float64bits(math.SmallestNonzeroFloat64)), bits(math.Float64bits(math.MaxFloat64)), bits(math.Float64bits(1)), bits(1 << 63),
	}
	for _, data := range seeds {
		for form := range uint8(forms) {
			f.Add(data, form, uint8(0))
		}
		f.Add(data, uint8(formBlobLiteral+forms), uint8(0))
	}
	// Values about the length limit of 1001 bytes, where a value or its record is too long.
	for n := 995; n <= 1002; n++ {
		for _, form := range []uint8{formTextArgument, formBlobArgument, formStringLiteral, formBlobLiteral + forms} {
			f.Add([]byte(strings.Repeat("x", n)), form, uint8(1))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, form, limit uint8) {
		n := 0
		if limit > 0 {
			n = 1000 + int(limit)
		}
		i := newInstance(t, n)
		p := newPair(t, i, nil)
		for _, s := range typesSetUp {
			if failure := p.step(stmttest.Call{Src: s, Supported: true}); failure != "" {
				t.Fatalf("the set-up: %v", failure)
			}
		}
		for _, table := range typesTables {
			c := typesCall(table, data, form)
			for range 2 {
				if failure := p.step(c); failure != "" {
					t.Fatal(failure)
				}
			}
			if failure := p.step(stmttest.Call{Src: "select * from " + table, Query: true, Supported: true}); failure != "" {
				t.Fatal(failure)
			}
		}
		want := p.o.state(t)
		if got := i.state(); got != want {
			t.Fatalf("the engine holds\n%v\nand SQLite\n%v", got, want)
		}
		if i.memory != nil {
			return
		}
		i.restart(true, engine.Options{})
		if got := i.state(); got != want {
			t.Fatalf("the engine holds after a replay into an empty cache\n%v\nand SQLite\n%v", got, want)
		}
	})
}

// whereTypes are the types of the tables of [FuzzWhere], and whereRows the keys of the rows of the
// table of each type, at the edges of the type: for a real -0.0 is the key 0.0, which SQLite stores
// for it.
var (
	whereTypes = []string{"int", "integer", "real", "text", "blob"}
	whereRows  = map[string][]any{
		"int":     {int64(math.MinInt64), int64(-1), int64(0), int64(1), int64(2), int64(math.MaxInt64)},
		"integer": {int64(math.MinInt64), int64(math.MinInt64 + 1), int64(0), int64(7), int64(math.MaxInt64 - 1), int64(math.MaxInt64)},
		"real":    {-math.MaxFloat64, -1.5, 0.0, math.SmallestNonzeroFloat64, 1.0, 1.5, math.MaxFloat64},
		"text":    {"", "\x01", "a", "a\x01", "ab", "b", "é", "\U0010ffff"},
		"blob":    {[]byte{}, []byte{0}, []byte{0, 0}, []byte{0, 1}, []byte("a"), []byte{0xff}, []byte{0xff, 0}},
	}
)

// FuzzWhere runs a select with a WHERE that the bytes of its input build on the engine, on a
// database in memory, a test double of the core, and on SQLite, and compares what each gives, as
// [TestDifferential] does, also with every row read: see [pair.bothWays]. The table, chosen by the
// first byte, has a key and a column v of one of the five types, which takes NULL, and rows whose
// keys are at the edges of the type, of which one in three has v NULL and the others v as the key.
// The input builds the conditions as [whereOf] says. [FuzzWhere_core] runs the same on the storage
// core.
func FuzzWhere(f *testing.F) {
	fuzzWhere(f, newMemoryInstance)
}

// FuzzWhere_core is [FuzzWhere] on the storage core, with a store in memory.
func FuzzWhere_core(f *testing.F) {
	fuzzWhere(f, func(t testing.TB) *instance { return newInstance(t, engine.Options{}) })
}

// fuzzWhere is [FuzzWhere] on the instances of newInstance. Its seeds have every operator on the key
// and on v, with a value of a row, of the bytes, NULL, and of another type, as a literal and as an
// argument, a column that the table does not have, and several conditions.
func fuzzWhere(f *testing.F, newInstance func(t testing.TB) *instance) {
	for table := range byte(len(whereTypes)) {
		for op := range byte(10) {
			for form := range byte(8) {
				if (op+form)%3 == 0 {
					f.Add([]byte{table, form / 2 % 3, op, form, 2, 1, 0})
				}
			}
		}
		f.Add([]byte{table, 0, 4, 0, 1, 0, 2, 0, 3, 3, 0, 2, 5})
		f.Add([]byte{table, 2, 0, 0, 0, 0, 6, 1, 0, 5, 9})
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}
		typ := whereTypes[int(data[0])%len(whereTypes)]
		i := newInstance(t)
		p := newPair(t, i, nil)
		table := "w_" + typ
		setUp := []stmttest.Call{{Src: "create table " + table + " (k " + typ + " primary key, v " + typ + ") strict, without rowid", Supported: true}}
		for j, key := range whereRows[typ] {
			v := key
			if j%3 == 1 {
				v = nil
			}
			setUp = append(setUp, stmttest.Call{Src: "insert into " + table + " values (?, ?)", Args: []any{key, v}, Supported: true})
		}
		for _, c := range setUp {
			if failure := p.step(c); failure != "" {
				t.Fatalf("the set-up: %v", failure)
			}
		}
		c := whereOf(table, typ, data[1:])
		if failure := p.step(c); failure != "" {
			t.Fatal(failure)
		}
	})
}

// whereOf the bytes data: a select of * from the table, whose key k and column v are of the type
// typ, with a WHERE of up to eight conditions, each of which takes bytes from data while it has
// them, and zero bytes after them: its column, k, v, or x, which the table does not have, in double
// quotes for a byte above 127; its operator, one of the eight of slice 7; and for a comparison its
// value, by a byte whose lowest bits choose a key of a row of the table, a value of typ from the
// bytes that follow, NULL, or a value of another type from the bytes that follow, and whose next bit
// chooses a literal or an argument.
func whereOf(table, typ string, data []byte) stmttest.Call {
	r := &fuzzBytes{data: data}
	where, args := r.conditions(typ, []string{"k", "v", "x"}, 8, nil)
	return stmttest.Call{Src: "select * from " + table + " where " + where, Args: args, Query: true, Supported: true}
}

// fuzzBytes are the bytes of the input of a fuzz target, which its methods take from the front, and
// which are zero bytes once they are all taken.
type fuzzBytes struct {
	data []byte
}

// next byte.
func (r *fuzzBytes) next() byte {
	if len(r.data) == 0 {
		return 0
	}
	b := r.data[0]
	r.data = r.data[1:]
	return b
}

// take n bytes, or what is left.
func (r *fuzzBytes) take(n int) []byte {
	n = min(n, len(r.data))
	b := r.data[:n]
	r.data = r.data[n:]
	return b
}

// conditions of a WHERE, without the keyword, of at least one and up to most conditions, the later
// ones while bytes are left, on columns of the names, of a table whose key k and other columns are
// of the type typ, after the arguments args of the parameters before them, and the arguments with
// theirs, as [whereOf] says.
func (r *fuzzBytes) conditions(typ string, names []string, most int, args []any) (string, []any) {
	ops := []string{"=", "==", "!=", "<>", "<", "<=", ">", ">=", "is null", "is not null"}
	var b strings.Builder
	for n := range most {
		if n > 0 {
			if len(r.data) == 0 {
				break
			}
			b.WriteString(" and ")
		}
		col := r.next()
		name := names[int(col&0x7f)%len(names)]
		if col > 127 {
			name = `"` + name + `"`
		}
		op := ops[int(r.next())%len(ops)]
		b.WriteString(name + " " + op)
		if strings.HasPrefix(op, "is") {
			continue
		}
		var term string
		term, args = r.term(typ, args)
		b.WriteString(" " + term)
	}
	return b.String(), args
}

// term of a value, after the arguments args of the parameters before it, and the arguments with its
// own: by a byte whose lowest bits choose a key of a row of the table of [whereRows] of the type typ,
// a value of typ from the bytes that follow, NULL, or a value of another type from the bytes that
// follow, and whose next bit chooses a literal or a ? with the value as its argument.
func (r *fuzzBytes) term(typ string, args []any) (string, []any) {
	form := r.next()
	var value any
	switch form % 4 {
	case 0:
		keys := whereRows[typ]
		value = keys[int(r.next())%len(keys)]
	case 1:
		value = whereValue(typ, r.take)
	case 3:
		// A type whose values are of another kind: INT and INTEGER hold one kind.
		others := slices.DeleteFunc(slices.Clone(whereTypes), func(o string) bool {
			return o == typ || strings.HasPrefix(o, "int") && strings.HasPrefix(typ, "int")
		})
		value = whereValue(others[int(r.next())%len(others)], r.take)
	}
	if form/4%2 == 1 {
		return "?", append(args, value)
	}
	return whereLiteral(value), args
}

// FuzzChanges runs updates and deletes that the bytes of its input build on the engine, on a database
// in memory, a test double of the core, and on SQLite, of their own or in a write transaction that
// commits or rolls back, and compares what each gives, as [TestDifferential] does, also with the rows
// read the other way: see [pair.bothWays]. The table, chosen by the first byte, has a key k and the
// columns v, which takes NULL, and w, which does not, of one of the five types, and the rows of
// [whereRows], of which one in three has v NULL and the others v as the key, and w is the key. The
// input builds the statements as [changesOf] says, and the tables are compared at the end.
// [FuzzChanges_core] runs the same on the storage core, and replays the log into an empty cache.
func FuzzChanges(f *testing.F) {
	fuzzChanges(f, newMemoryInstance)
}

// FuzzChanges_core is [FuzzChanges] on the storage core, with a store in memory.
func FuzzChanges_core(f *testing.F) {
	fuzzChanges(f, func(t testing.TB) *instance { return newInstance(t, engine.Options{}) })
}

// fuzzChanges is [FuzzChanges] on the instances of newInstance. Its seeds have updates of each column
// and of several, also of a column that the table does not have, with each kind of value, as a
// literal and as an argument, with and without WHERE, deletes with and without WHERE, of one row and
// of many, a change of the key that meets a key that exists, and several statements in a transaction.
func fuzzChanges(f *testing.F, newInstance func(t testing.TB) *instance) {
	for table := range byte(len(whereTypes)) {
		for mask := range byte(16) {
			for form := range byte(8) {
				if (mask+form)%5 == 0 {
					// An update of the columns of the mask, each with a value of the form, and a WHERE of one
					// condition = on the key.
					data := []byte{table, 0, 0b010}
					data = append(data, mask)
					for range 4 {
						data = append(data, form, 2, 1)
					}
					f.Add(append(data, 0, 0, 0, 3))
				}
			}
		}
		f.Add([]byte{table, 1, 0b001, 0, 6, 0, 0, 2})                   // In a transaction that rolls back, a delete of the rows with k >= a key.
		f.Add([]byte{table, 3, 0b000, 0b0001, 0, 1, 0b011, 2, 6, 0, 1}) // In one that commits: an update of k to a key that exists, and a delete.
		f.Add([]byte{table, 0, 0b001, 0b100, 0, 0, 2, 0b000, 0b0010, 1, 0b111, 0b1000, 4, 3, 0, 0, 2})
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}
		typ := whereTypes[int(data[0])%len(whereTypes)]
		i := newInstance(t)
		p := newPair(t, i, nil)
		table := "c_" + typ
		setUp := []stmttest.Call{{Src: "create table " + table + " (k " + typ + " primary key, v " + typ + ", w " + typ + " not null) strict, without rowid", Supported: true}}
		for j, key := range whereRows[typ] {
			v := key
			if j%3 == 1 {
				v = nil
			}
			setUp = append(setUp, stmttest.Call{Src: "insert into " + table + " values (?, ?, ?)", Args: []any{key, v, key}, Supported: true})
		}
		for _, c := range setUp {
			if failure := p.step(c); failure != "" {
				t.Fatalf("the set-up: %v", failure)
			}
		}
		inTx, commit := data[1]&1 == 1, data[1]&2 == 2
		var log []string
		if inTx {
			log = append(log, "-- begin")
			is.NotError(t, i.c.Begin(t.Context()))
			is.NotError(t, p.other.c.Begin(t.Context()))
			for _, s := range oracleBegin(false) {
				is.Equal(t, "", p.o.run(t, s).err, s)
			}
		}
		for _, c := range changesOf(table, typ, data[2:]) {
			log = append(log, fmt.Sprintf("%v -- with %#v", c.Src, c.Args))
			if failure := p.step(c); failure != "" {
				t.Fatalf("after\n%v\n%v", strings.Join(log[:len(log)-1], "\n"), failure)
			}
		}
		if inTx {
			log = append(log, fmt.Sprint("-- end, with a commit: ", commit))
			for _, c := range []*engine.Conn{i.c, p.other.c} {
				if commit {
					is.NotError(t, c.Commit(t.Context()))
				} else {
					is.NotError(t, c.Rollback(t.Context()))
				}
			}
			for _, s := range oracleEnd(false, commit) {
				is.Equal(t, "", p.o.run(t, s).err, s)
			}
		}
		want := p.o.state(t)
		if got := i.state(); got != want {
			t.Fatalf("after\n%v\nthe engine holds\n%v\nand SQLite\n%v", strings.Join(log, "\n"), got, want)
		}
		if got := p.other.state(); got != want {
			t.Fatalf("after\n%v\nthe engine that reads the rows the other way holds\n%v\nand SQLite\n%v", strings.Join(log, "\n"), got, want)
		}
		if i.memory != nil {
			return
		}
		i.restart(true, engine.Options{})
		if got := i.state(); got != want {
			t.Fatalf("after\n%v\nthe engine holds after a replay into an empty cache\n%v\nand SQLite\n%v", strings.Join(log, "\n"), got, want)
		}
	})
}

// changesOf the bytes data: up to four updates and deletes of the table, whose key k and columns v
// and w are of the type typ, while bytes are left, each by a byte whose lowest bit chooses a delete
// or an update and whose next two bits the number of conditions of its WHERE, none for no WHERE.
// An update sets the columns of the lowest four bits of the next byte, k, v, w, and x, which the table
// does not have, or v for none, in that order, or in the reverse order if the next bit is set, and in
// double quotes if the one after is, each with a value as [fuzzBytes.term] gives it. The conditions
// are as [whereOf] has them, on k, v, w, or x.
func changesOf(table, typ string, data []byte) []stmttest.Call {
	r := &fuzzBytes{data: data}
	var calls []stmttest.Call
	for n := 0; n < 4 && (n == 0 || len(r.data) > 0); n++ {
		kind := r.next()
		var b strings.Builder
		c := stmttest.Call{Supported: true}
		if kind&1 == 1 {
			b.WriteString("delete from " + table)
		} else {
			b.WriteString("update " + table + " set ")
			mask := r.next()
			columns := []string{"k", "v", "w", "x"}
			var set []string
			for j, name := range columns {
				if mask&(1<<j) != 0 {
					set = append(set, name)
				}
			}
			if set == nil {
				set = []string{"v"}
			}
			if mask&16 != 0 {
				slices.Reverse(set)
			}
			for j, name := range set {
				if j > 0 {
					b.WriteString(", ")
				}
				if mask&32 != 0 {
					name = `"` + name + `"`
				}
				var term string
				term, c.Args = r.term(typ, c.Args)
				b.WriteString(name + " = " + term)
			}
		}
		if conditions := int(kind>>1) % 4; conditions > 0 {
			var where string
			where, c.Args = r.conditions(typ, []string{"k", "v", "w", "x"}, conditions, c.Args)
			b.WriteString(" where " + where)
		}
		c.Src = b.String()
		calls = append(calls, c)
	}
	return calls
}

// whereValue of the type typ from the bytes that take gives: an integer or the bits of a real of up
// to eight bytes, and text, valid UTF-8 without NUL, or a blob, of as many bytes as the first byte
// says, up to 15. A real is finite.
func whereValue(typ string, take func(n int) []byte) any {
	switch typ {
	case "int", "integer", "real":
		var bits [8]byte
		copy(bits[:], take(8))
		v := binary.LittleEndian.Uint64(bits[:])
		if typ != "real" {
			return int64(v)
		}
		if f := math.Float64frombits(v); !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
		return float64(int64(v))
	}
	n := 0
	if b := take(1); len(b) > 0 {
		n = int(b[0] % 16)
	}
	data := slices.Clone(take(n))
	if typ == "blob" {
		return data
	}
	return strings.ReplaceAll(strings.ToValidUTF8(string(data), "?"), "\x00", "0")
}

// whereLiteral of the value v of [whereValue], as SQL writes it, or NULL for nil.
func whereLiteral(v any) string {
	switch v := v.(type) {
	case int64:
		return fmt.Sprint(v)
	case float64:
		return strconv.FormatFloat(v, 'e', -1, 64)
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case []byte:
		return "x'" + hex.EncodeToString(v) + "'"
	}
	return "null"
}
