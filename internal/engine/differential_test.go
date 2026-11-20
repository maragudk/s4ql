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
	i := newInstanceLimited(t, engine.Options{}, nil, limit)
	defer i.close()
	differentialOn(t, seed, n, restarts, y, i)
}

// differentialOn runs the sequence of n statements from seed on the instance i, as [differential]
// does.
func differentialOn(t testing.TB, seed uint64, n int, restarts bool, y tally, i *instance) {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, 0xe1e))
	q := newSequence(t, r)
	p := newPair(t, i, y)
	var log []string
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("seed %v, after\n%v\n%v", seed, strings.Join(log, "\n"), fmt.Sprintf(format, args...))
	}
	for range n {
		if restarts && r.IntN(10) == 0 {
			empty := r.IntN(2) == 0
			log = append(log, fmt.Sprint("-- restart, with an empty cache: ", empty))
			i.restart(empty, engine.Options{})
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
	if got, want := i.state(), p.o.state(t); got != want {
		fail("at the end the engine holds\n%v\nand SQLite\n%v", got, want)
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
	y        tally
}

// newPair of the instance i and SQLite, with the length limit of i.
func newPair(t testing.TB, i *instance, y tally) *pair {
	t.Helper()
	p := &pair{t: t, i: i, o: newOracle(t), empty: newOracle(t), y: y}
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
	p.y.add(src, got)
	p.reach(c, got)
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
	want := o.run(t, src, args...)
	if !c.Query {
		want.columns, want.rows = "", nil
	}
	if got.String() != want.String() {
		gap := knownGap(src, got, want)
		switch {
		case startedTable(t, p.empty, src, got, want):
			p.y.excused(acceptedStartedTable)
		case gap != "":
			p.y.excused(gap + knownGapSuffix)
		default:
			return fmt.Sprintf("%q with the arguments %v: the engine gives\n%v\nand SQLite\n%v", src, args, got, want)
		}
	}
	// SQLite's count of changes: the engine's result sets it, and a statement that fails keeps it,
	// except one that fails while SQLite steps it, which sets it to 0: a broken constraint, an insert
	// in a read-only transaction, and an insert of a row whose record is too long, while its value,
	// which SQLite would refuse when it binds it, is not.
	changes, rowid := o.changes(t)
	p.prev = changes
	recordTooBig := want.err == tooBig && isInsert(src) && !slices.ContainsFunc(args, func(a any) bool { return byteLength(a) > p.i.limit })
	switch {
	case strings.HasPrefix(want.err, fmt.Sprint(sqlerr.CodeConstraint, "/")), strings.HasPrefix(want.err, fmt.Sprint(sqlerr.CodeReadOnly, "/")) && isInsert(src), recordTooBig:
		if changes != 0 {
			return fmt.Sprintf("%q: SQLite counts %v changes after a broken constraint, a refused insert, or a record that is too long", src, changes)
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

// reach counts in the tally what the call c, whose answer of the engine is got, reaches, under
// "reached: ", on the database of SQLite before the call, as [reach] says.
func (p *pair) reach(c stmttest.Call, got answer) {
	p.t.Helper()
	if p.y != nil {
		reach(p.t, p.o, c, got, func(what string) { p.y["reached: "+what]++ })
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
// the values of its rows.
func reach(t testing.TB, o *oracle, c stmttest.Call, got answer, count func(string)) {
	t.Helper()
	outcome := "ok"
	switch {
	case got.err == "not supported" && strings.Contains(got.message, " for the column "):
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
	case *stmt.Select:
		_, columns := o.columns(t, s.Table.Value)
		if columns == nil {
			return
		}
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
// transaction the rule decides nothing: SQLite and the engine refuse the write before they look at
// its value.
func (p *pair) checkType(c stmttest.Call, args []any, got answer) (excuse, failure string) {
	t := p.t
	t.Helper()
	if p.readOnly || slices.ContainsFunc(args, func(a any) bool { return p.i.limit > 0 && byteLength(a) > p.i.limit }) {
		// An argument that is too long gets SQLite's error when SQLite binds it, which the caller
		// compares.
		return "", ""
	}
	want := typeRule(t, p.o, c.Src, args)
	if want == "" {
		return "", ""
	}
	if got.err != "not supported" || got.message != want {
		return excusedType, fmt.Sprintf("%q with the arguments %#v: the engine gives %v (%q), and the rule %q", c.Src, args, got, got.message, want)
	}
	return excusedType, ""
}

// checkLength of the call c, whose answer of the engine is got, against s4ql's rule for a literal
// that is longer than the length limit: an insert that SQLite prepares fails with SQLite's `string or
// blob too big`, after the names. SQLite meets such a literal only with a limit below the length of
// the text that holds it, and then gives the error for a string literal only when it steps the
// statement, and turns a blob literal into NULL, so the call is not run in SQLite. It gives
// [excusedLong] if the rule decides the call, or "", and what differs from the rule, or "".
func (p *pair) checkLength(c stmttest.Call, got answer) (excuse, failure string) {
	t := p.t
	t.Helper()
	s, _ := stmt.Check(c.Src)
	insert, ok := s.(*stmt.Insert)
	if !ok || p.i.limit == 0 || p.o.prepare(t, c.Src) != nil {
		return "", ""
	}
	var n int
	for _, v := range insert.Values {
		n = max(n, byteLength(v.Value))
	}
	if n <= p.i.limit {
		return "", ""
	}
	if got.err != tooBig {
		return excusedLong, fmt.Sprintf("%q with a literal of %v bytes: the engine gives %v, and the rule %q", c.Src, n, got, tooBig)
	}
	return excusedLong, ""
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

// typeRule of the insert of the text src, with the arguments args as SQLite binds them, on the
// database of o: s4ql's "not supported" for the first column, in the order of the table, whose
// value, a literal or an argument, is of another type than the column, where SQLite would convert
// the value or give an error of its own, as [stmttest.TypeRule] gives it, or "". It is "" for a text
// that is not an insert, an insert that SQLite does not prepare, which gets SQLite's error, an insert
// with another number of arguments than parameters, an insert that leaves a column without a value,
// which gets SQLite's `NOT NULL constraint failed` first, and an insert whose every value has the
// type of its column.
func typeRule(t testing.TB, o *oracle, src string, args []any) string {
	t.Helper()
	s, _ := stmt.Check(src)
	insert, ok := s.(*stmt.Insert)
	if !ok || len(args) != insert.Parameters() || o.prepare(t, src) != nil {
		return ""
	}
	table, columns := o.columns(t, insert.Table.Value)
	return stmttest.TypeRule(table, columns, insert, args)
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

// isInsert reports whether src is a supported insert.
func isInsert(src string) bool {
	s, _ := stmt.Check(src)
	_, ok := s.(*stmt.Insert)
	return ok
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
		if y[kind] == 0 {
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
		// no longer among [stmttest.Unsupported].
		want := map[string]int{acceptedStartedTable: 18, excusedArguments: 703, excusedArgument: 220, excusedType: 339, excusedLong: 8}
		kinds := slices.Collect(maps.Keys(want))
		for name := range y {
			if kind, ok := strings.CutPrefix(name, "excused: "); ok && !slices.Contains(kinds, kind) {
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
	"-0.0 literal into REAL UNIQUE constraint failed": 6, "-0.0 argument into REAL UNIQUE constraint failed": 2,
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
	"select * with NULL ok": 45, "select of names with NULL ok": 25,
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
