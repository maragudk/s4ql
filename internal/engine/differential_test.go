package engine_test

import (
	"context"
	"database/sql"
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
	// columns of a query, each as its name and declared type, and its rows, each as its values.
	columns string
	rows    []string
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

// run the statement of src, as sqlite3_prepare_v2 and sqlite3_step run it, with args bound to its
// parameters by position: each an int64, a float64, a string, or a []byte.
func (o *oracle) run(t testing.TB, src string, args ...any) answer {
	t.Helper()
	s, _, err := o.c.PrepareFirst(src)
	if err != nil {
		return answer{err: sqliteText(t, err)}
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
		default:
			t.Fatalf("%q: an argument of type %T for SQLite", src, a)
		}
		if err != nil {
			return answer{err: sqliteText(t, err)}
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
			return answer{err: sqliteText(t, err)}
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
				values[i] = "null"
			default:
				t.Fatalf("SQLite gives a value of type %v", typ)
			}
		}
		a.rows = append(a.rows, strings.Join(values, ","))
	}
}

// state of the database in SQLite: each table, in the order of the names as bytes, with its
// columns and its rows in the order of select *.
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
		fmt.Fprintf(&b, "%v (%v): %v\n", name, a.columns, strings.Join(a.rows, " "))
	}
	return b.String()
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
	return answer{err: errText(t, src, err), message: err.Error()}
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
// driver: an integer for a Go integer that fits a signed 64-bit integer, which the driver converts,
// the value of a [sql.NamedArg], whose name the driver refuses before the engine, and for every other
// argument a value that is not an integer, which the driver never hands the engine: text, a real, a
// blob, NULL, and a value of no kind for a type that has no value of the engine.
func values(args []any) []*s4qlpb.Value {
	var vs []*s4qlpb.Value
	for _, a := range unnamed(args) {
		if v, ok := stmttest.Integers([]any{a}); ok {
			vs = append(vs, s4qlpb.Value_builder{Integer: new(v[0])}.Build())
			continue
		}
		var b s4qlpb.Value_builder
		switch a := a.(type) {
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
			columns = append(columns, c.GetName()+" "+declType(c.GetType()))
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
	for _, kind := range []string{"already exists", "no such table", "has no column named", "UNIQUE constraint failed", "syntax error", "incomplete input"} {
		if strings.Contains(err, kind) {
			return kind
		}
	}
	return err
}

// differential runs the sequence of n statements from seed on the engine and on SQLite, and compares
// after each statement what each gives, the count of changes, and the last insert rowid, and at the
// end, and after each restart, every table. With restarts, the instance is closed and opened again
// at random points, with an empty cache or with its cache.
func differential(t testing.TB, seed uint64, n int, restarts bool, y tally) {
	t.Helper()
	// The instance closes at the end of the sequence, so that a test of many sequences does not keep
	// the files of each open until it ends.
	i := newInstance(t, engine.Options{})
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
	y    tally
}

func newPair(t testing.TB, i *instance, y tally) *pair {
	t.Helper()
	p := &pair{t: t, i: i, o: newOracle(t), empty: newOracle(t), y: y}
	t.Cleanup(p.o.close)
	t.Cleanup(p.empty.close)
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
	if kind, failure := p.checkType(c, args, got); kind != "" {
		if failure == "" {
			p.y.excused(kind)
		}
		return failure
	}
	if got.err == "not supported" {
		// A statement of slices 1 and 2 is supported, and a text that is not supported is one that
		// SQLite accepts: SQLite prepares it on an empty database without a syntax error.
		if c.Supported {
			return fmt.Sprintf("%q: the engine does not support a statement of slice 1: %v", src, got.message)
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
	// except one that fails while SQLite steps it, which sets it to 0: a broken constraint, and an
	// insert in a read-only transaction.
	changes, rowid := o.changes(t)
	p.prev = changes
	switch {
	case strings.HasPrefix(want.err, fmt.Sprint(sqlerr.CodeConstraint, "/")), strings.HasPrefix(want.err, fmt.Sprint(sqlerr.CodeReadOnly, "/")) && isInsert(src):
		if changes != 0 {
			return fmt.Sprintf("%q: SQLite counts %v changes after a broken constraint or a refused insert", src, changes)
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
	switch a := a.(type) {
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
		return "is NULL"
	}
	return "has no value"
}

// oracleArgs of the arguments args of a call, as SQLite binds them: each Go integer as an int64, and
// every other argument as it is, with the names dropped.
func oracleArgs(args []any) []any {
	var as []any
	for _, a := range unnamed(args) {
		if v, ok := stmttest.Integers([]any{a}); ok {
			a = v[0]
		}
		as = append(as, a)
	}
	return as
}

// checkType of the call c, with the arguments args as SQLite binds them, whose answer of the engine is
// got, against s4ql's rule for the type of a value: an insert that SQLite prepares, whose value, a
// literal or an argument, is of another type than its column, is "not supported", where SQLite would
// convert the value or give an error of its own. It gives [excusedType] if the rule decides the call,
// or "", and what differs from the rule, or "". An insert that SQLite does not prepare gets SQLite's
// error, which the caller compares.
func (p *pair) checkType(c stmttest.Call, args []any, got answer) (excuse, failure string) {
	t := p.t
	t.Helper()
	s, _ := stmt.Check(c.Src)
	insert, ok := s.(*stmt.Insert)
	if !ok || p.o.prepare(t, c.Src) != nil {
		return "", ""
	}
	table, column, typ := p.o.column(t, insert.Table.Value)
	var what, kind string
	switch v := insert.Value.(type) {
	case nil:
		if len(args) != 1 {
			return "", ""
		}
		var article string
		switch args[0].(type) {
		case int64:
			kind, article = "integer", "an integer"
		case float64:
			kind, article = "real", "a real"
		case string:
			kind, article = "text", "text"
		case []byte:
			kind, article = "blob", "a blob"
		default:
			t.Fatalf("%q: an argument of type %T for SQLite", c.Src, args[0])
		}
		what = fmt.Sprintf("argument 1, which is %v,", article)
	case stmt.Integer:
		kind, what = "integer", "an integer literal"
	case stmt.Real:
		kind, what = "real", "a real literal"
	case stmt.Text:
		kind, what = "text", "a string literal"
	case stmt.Blob:
		kind, what = "blob", "a blob literal"
	default:
		t.Fatalf("%q: a value of type %T", c.Src, v)
	}
	if columnHolds[typ] == kind {
		return "", ""
	}
	want := fmt.Sprintf("s4ql: not supported: %v for the column %v.%v of type %v", what, table, column, typ)
	if got.err != "not supported" || got.message != want {
		return excusedType, fmt.Sprintf("%q with the arguments %#v: the engine gives %v (%q), and the rule %q", c.Src, args, got, got.message, want)
	}
	return excusedType, ""
}

// columnHolds is the kind of value that a column of each declared type holds.
var columnHolds = map[string]string{"INT": "integer", "INTEGER": "integer", "REAL": "real", "TEXT": "text", "BLOB": "blob"}

// column of the table with the name, as SQLite compares names, in SQLite: the names of the table and
// of its primary-key column as the table stores them, and its declared type. The table must exist.
func (o *oracle) column(t testing.TB, name string) (table, column, typ string) {
	t.Helper()
	s, err := o.c.Prepare("select m.name, p.name, p.type from sqlite_schema m, pragma_table_info(m.name) p where m.type = 'table' and m.name = ?1 collate nocase and p.pk = 1")
	is.NotError(t, err)
	defer func() { is.NotError(t, s.Finalize()) }()
	is.NotError(t, s.BindText(1, name))
	ok, err := s.Step()
	is.NotError(t, err)
	if !ok {
		t.Fatalf("SQLite has no table %q", name)
	}
	return s.ColumnText(0), s.ColumnText(1), s.ColumnText(2)
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
// restarts of the instance, with an empty cache and with its own.
func TestDifferential(t *testing.T) {
	n := *sequences
	if slow() {
		n /= 10
	}
	y := tally{}
	for s := range uint64(n) {
		differential(t, *seed+s, *length, s%2 == 1, y)
	}
	t.Logf("%v sequences of %v statements: %v", n, *length, y)
	// The outcomes of a parameter that the run must reach: an insert through a parameter that
	// succeeds, that meets a key that exists, and that the rule for arguments refuses, and an argument
	// for a statement without a parameter.
	for _, kind := range []string{
		"insert with a parameter ok", "insert with a parameter UNIQUE constraint failed", "insert with a parameter wrong number of arguments",
		"insert with a parameter not supported", "insert wrong number of arguments", "select wrong number of arguments",
	} {
		if y[kind] == 0 {
			t.Errorf("no statement has the outcome %q", kind)
		}
	}
	// The texts that the full run excuses from the comparison with SQLite, by kind. Every other kind,
	// such as each known gap, is pinned at 0. Of the calls that are excused because s4ql refuses
	// their arguments by its rule, each is checked against the rule instead.
	if !slow() && atDefault("seed", "sequences", "length") {
		want := map[string]int{acceptedStartedTable: 24, excusedArguments: 632, excusedArgument: 50, excusedType: 10}
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
// of statements, and whether the instance restarts, as [TestDifferential] does.
func FuzzDifferential_core(f *testing.F) {
	f.Add(uint64(0), uint8(40), false)
	f.Add(uint64(1), uint8(40), true)
	f.Fuzz(func(t *testing.T, seed uint64, n uint8, restarts bool) {
		differential(t, seed, int(n%64), restarts, nil)
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
