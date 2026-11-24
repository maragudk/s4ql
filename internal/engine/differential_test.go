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
	"maragu.dev/s4ql/internal/testshard"
)

var (
	seed      = flag.Uint64("seed", 0, "first seed of the sequences of statements of TestDifferential, each with the profile of its seed, in place of the seeds of each profile")
	sequences = flag.Int("sequences", 40, "sequences of statements of each profile of TestDifferential, and twice as many of some: see sequencesOf")
	length    = flag.Int("length", 48, "statements in each sequence of TestDifferential")
	scanAll   = flag.Bool("scanall", false, "read every row of its table in each query of TestDifferential and FuzzDifferential_core, and not the rows by the key")
	// directions replays a failure that depends on the directions in which the queries read: a test
	// that fails logs the seed of each of its instances.
	directions = flag.Uint64("directions", 0, "seed of the directions of the scans of the queries of every instance of the tests, or 0 for a seed that each instance draws")
)

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
	// access of the rows of a query of the engine, as [engine.Rows.Access] gives it with the
	// direction that the engine drew, or nil. It is not in the text of the answer.
	access *engine.Access
	// backward is the direction of the rows of a statement with RETURNING of the engine, as
	// [engine.Rows.Backward] gives it, or nil. It is not in the text of the answer.
	backward *bool
}

// String of the answer, which is what the tests compare between the engine and SQLite: its error, or
// its columns and its rows as a multiset. SQLite leaves the order of the rows of a select without
// ORDER BY unspecified, and the engine varies it from one query to the next, so no comparison is by
// the order of the rows: the text holds the rows in the order of their texts, each in quotes. Two
// answers have one text only if they have the same rows the same number of times: a row that is
// wrong changes an element of the sorted list, and a row that is missing, or one too many, also one
// that is there twice, changes its length or another element. The rows field keeps the order in
// which the rows came.
func (a answer) String() string {
	if a.err != "" {
		return a.err
	}
	return fmt.Sprintf("columns %q, rows %q", a.columns, slices.Sorted(slices.Values(a.rows)))
}

// oracle is SQLite, on a database in memory, which runs the statements that the engine runs.
type oracle struct {
	c *sqlite.Conn
	// counts gives SQLite's count of changes and its last insert rowid.
	counts *sqlite.Stmt
	// limit is the length limit of c, 0 for SQLite's default.
	limit int
}

// lengthLimit of SQLite, n bytes, 0 for SQLite's default.
func (o *oracle) lengthLimit(t testing.TB, n int) {
	t.Helper()
	o.limit = n
	if n == 0 {
		n = math.MaxInt32
	}
	is.NotError(t, o.c.SetLengthLimit(n))
}

// unlimited runs src with args as [oracle.run] does, without the length limit, for a query of a test
// itself, such as the rows of a select in another order, which SQLite sorts with a record of its own
// under the limit.
func (o *oracle) unlimited(t testing.TB, src string, args ...any) answer {
	t.Helper()
	if o.limit == 0 {
		return o.run(t, src, args...)
	}
	is.NotError(t, o.c.SetLengthLimit(math.MaxInt32))
	defer func() { is.NotError(t, o.c.SetLengthLimit(o.limit)) }()
	return o.run(t, src, args...)
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
// columns, as [columnText] gives them, and its rows in the order of its primary key.
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
		_, columns := o.columns(t, name)
		var cs, key []string
		for _, c := range columns {
			cs = append(cs, columnText(c.Name, c.Type, c.Nullable, o.defaultText(t, c.Default)))
			if c.PrimaryKey {
				key = append(key, stmttest.Quote(c.Name))
			}
		}
		// In the order of the key, as the state of the engine is: without ORDER BY, SQLite reads the
		// rows in the order of an index, where one holds every column of the table.
		a := o.run(t, "select * from "+stmttest.Quote(name)+" order by "+strings.Join(key, ", "))
		fmt.Fprintf(&b, "%v (%v): %v\n", name, strings.Join(cs, ", "), strings.Join(a.rows, " "))
	}
	return b.String() + o.indexes(t)
}

// columnText of a column of a table in the text of a state: its name and its declared type, NULL
// after them if it takes NULL, and DEFAULT and the text of the value of its default, dflt, as
// [valueText] writes it, if it has one, where dflt is not "".
func columnText(name, typ string, nullable bool, dflt string) string {
	text := name + " " + typ
	if nullable {
		text += " NULL"
	}
	if dflt != "" {
		text += " DEFAULT " + dflt
	}
	return text
}

// defaultText of a column whose default SQLite keeps as the text dflt, or "" for none: the value
// that SQLite gives the literal, as [oracle.run] writes it, which is what an insert that leaves the
// column out stores, but for -0.0, which a REAL column of a STRICT table stores as 0.0.
func (o *oracle) defaultText(t testing.TB, dflt string) string {
	t.Helper()
	if dflt == "" {
		return ""
	}
	a := o.run(t, "select "+dflt)
	if a.err != "" || len(a.rows) != 1 {
		t.Fatalf("SQLite gives %v for the default %q", a, dflt)
	}
	if a.rows[0] == "real -0" {
		return "real 0"
	}
	return a.rows[0]
}

// defaultOf a column of the commit format: the text of its default, as [valueText] writes it, or ""
// if it has none.
func defaultOf(t testing.TB, c *s4qlpb.Column) string {
	t.Helper()
	if !c.HasDefault() {
		return ""
	}
	return valueText(t, c.GetDefault())
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
	// seed of the directions in which the queries of the engines of the instance read, for options
	// that have none of their own: the engine of the n-th open has the seed plus n.
	seed  uint64
	opens uint64
	// planted is set for an instance whose engine has a planted bug, which can cause an internal error.
	planted bool
}

// seeded gives the instance the seed of the flag -directions, or one that it draws, and logs it if
// the test fails, so that the directions of the failure can be had again.
func (i *instance) seeded() {
	i.seed = *directions
	if i.seed == 0 {
		i.seed = sim.NewFree(sim.Buggify{}).Uint64()
	}
	i.t.Cleanup(func() {
		if i.t.Failed() {
			i.t.Logf("an instance of the engine drew the directions of its scans from the seed %v: run the test again with -directions=%v", i.seed, i.seed)
		}
	})
}

// directed is options with the seed of the next engine of the instance, unless they have one.
func (i *instance) directed(options engine.Options) engine.Options {
	if !options.Seeded() {
		options.Seed(i.seed + i.opens)
	}
	i.opens++
	return options
}

// newMemoryInstance of the engine on a database in memory.
func newMemoryInstance(t testing.TB) *instance {
	i := &instance{t: t}
	i.seeded()
	i.e, i.memory = engine.NewMemory(sim.NewFree(sim.Buggify{}), i.directed(engine.Options{}))
	i.c = i.e.Conn()
	return i
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
	return newInstanceSeeded(t, options, coreLog, limit, 0)
}

// newInstanceSeeded is [newInstanceLimited] whose engines draw the directions of their scans from
// seed, unless it is 0 or the flag -directions gives one, as [instance.seeded] says.
func newInstanceSeeded(t testing.TB, options engine.Options, coreLog *slog.Logger, limit int, seed uint64) *instance {
	t.Helper()
	h := sim.NewFree(sim.Buggify{})
	i := &instance{t: t, host: h, store: simstore.New(h, simstore.Options{}), dir: t.TempDir(), coreLog: coreLog, limit: limit}
	i.seeded()
	if seed != 0 && *directions == 0 {
		i.seed = seed
	}
	i.open(true, options)
	t.Cleanup(func() {
		i.close()
		is.NotError(t, i.c.Close(context.WithoutCancel(t.Context())))
	})
	return i
}

// newInstanceDirected is [newInstanceLimited] whose engines draw the directions of their scans from
// a seed that the seed of a sequence gives, unless the flag -directions gives one, so that what a
// generated test counts of the directions, such as the order of the rows that tie under a sort, is a
// function of the seeds of its sequences, as every other count is.
func newInstanceDirected(t testing.TB, options engine.Options, limit int, seed uint64) *instance {
	t.Helper()
	return newInstanceSeeded(t, options, nil, limit, seed*0x9e3779b97f4a7c15+0x5ca1ab1e)
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
	i.d, i.e = d, engine.New(d, i.host, i.directed(options))
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
		return i.errAnswer(src, err)
	}
	var a answer
	var columns []string
	for _, c := range rows.Columns() {
		columns = append(columns, c.Name+" "+c.DeclType)
	}
	a.columns = strings.Join(columns, ", ")
	if access, ok := rows.Access(); ok {
		a.access = &access
	}
	if backward, ok := rows.Backward(); ok {
		a.backward = &backward
	}
	for row, err := range rows.All() {
		if err != nil {
			return i.errAnswer(src, err)
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
		return i.errAnswer(src, err)
	}
	return answer{}
}

// errAnswer of the engine's error err for src, as [errAnswer] gives it, or, for an instance with a
// planted bug, an answer of the internal error that the bug can cause.
func (i *instance) errAnswer(src string, err error) answer {
	i.t.Helper()
	if i.planted && errors.Is(err, engine.ErrInternal) {
		return answer{err: "internal error", message: err.Error()}
	}
	return errAnswer(i.t, src, err)
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
			columns = append(columns, columnText(c.GetName(), declType(c.GetType()), c.GetNullable(), defaultOf(i.t, c)))
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
	indexes, err := indexesText(ctx, r, tables)
	is.NotError(i.t, err)
	return b.String() + indexes
}

// sequence of random statements of slice 1, as [stmttest.Sequence] gives them.
type sequence = stmttest.Sequence

func newSequence(t testing.TB, seed uint64) *sequence {
	t.Helper()
	q, err := stmttest.NewSequence(seed)
	is.NotError(t, err)
	return q
}

// newSequenceFrom the seed, after [newSequence] has succeeded once.
func newSequenceFrom(seed uint64) *sequence {
	q, err := stmttest.NewSequence(seed)
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

// comparedWithSQLite is the kind under which a tally counts the calls that ran in SQLite, whose
// answers [pair.step] compared with the engine's.
const comparedWithSQLite = "compared with SQLite"

// compared counts a call that runs in SQLite, under [comparedWithSQLite].
func (y tally) compared() {
	if y != nil {
		y[comparedWithSQLite]++
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
	// changes a row, and turns a blob literal into NULL: the known difference of docs/todo/sql-front-end.md. See
	// [pair.checkLength].
	excusedLongChange = "an update or a delete with a literal longer than the length limit, checked against s4ql's rule"
	// excusedRecordSlip is an update of two rows or more that s4ql refuses with SQLite's `string or
	// blob too big` for a record of a row, longer than the length limit by SQLite's measure of that row
	// alone, with no error of an earlier row, which SQLite stores where its allocator leaves room: the
	// known difference of
	// docs/todo/sql-front-end.md. See [pair.recordSlips]. Its count depends on the platform, so no test pins it.
	excusedRecordSlip = "an update of rows of which SQLite does not measure every record, checked by an update of each row alone"
	// excusedLongCondition is a select with a literal in a condition that is longer than the length
	// limit, which s4ql refuses with SQLite's `string or blob too big`, and for which SQLite gives
	// rows: no rows for a blob literal, which it turns into NULL, and for a string literal on the
	// upper bound of the key whose search reaches no row. See [pair.longCondition].
	excusedLongCondition = "a literal in a condition longer than the length limit, for which SQLite gives rows, checked against s4ql's rule"
	// excusedCacheLimit is a create table that SQLite takes under the length limit and whose
	// statement in the cache is longer than the limit, which s4ql refuses as not supported. See
	// [pair.tooLongForCache].
	excusedCacheLimit = "a create table whose statement in the cache is longer than the length limit, checked against the length of that statement"
	// excusedSortTooBig is a select with ORDER BY that SQLite refuses under the length limit with
	// `string or blob too big`, for the record of what it sorts of a row, and that s4ql answers with
	// rows: the known difference of slice 13, by the decision of the lead in docs/todo/slice-13.md.
	// The rows of s4ql must be SQLite's for the same select without the length limit. See
	// [pair.sortTooBig].
	excusedSortTooBig = "a sorted select that SQLite refuses for the length of what it sorts, checked against SQLite without the length limit"
	// excusedReturningTooBig is an insert, an update, or a delete whose RETURNING names a column twice,
	// which SQLite refuses with `string or blob too big` under a lowered length limit, where s4ql gives
	// the rows: SQLite writes each row of RETURNING into a record of its own, which a column named
	// twice makes longer than the row. The difference was found by the generated checks of slice 14
	// and waits for the lead's decision in docs/todo/slice-14.md. The statement without RETURNING must
	// succeed in SQLite under the limit, and s4ql's rows must be SQLite's without the limit. See
	// [pair.returningTooBig].
	excusedReturningTooBig = "a statement whose RETURNING names a column twice, which SQLite refuses for the length of a row of RETURNING, checked against SQLite without the length limit"
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
	i := newInstanceDirected(t, generatedOptions(), limit, seed)
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
	if failure := differentialFind(t, seed, n, restarts, y, i, generatedOptions()); failure != "" {
		t.Fatal(failure)
	}
}

// differentialFind runs the sequence of n statements from seed on the instance i, as [differential]
// does, with the options of the engine options after each restart, and gives the first difference
// from SQLite, with the statements before it, or "".
func differentialFind(t testing.TB, seed uint64, n int, restarts bool, y tally, i *instance, options engine.Options) (failure string) {
	t.Helper()
	q := newSequence(t, seed)
	// The restarts draw from a stream of their own, so that they move no statement.
	r := q.Stream("restarts")
	p := newPairWith(t, i, y, options)
	p.rnd = rand.New(rand.NewPCG(seed, 0x9a7))
	// The other instance closes at the end of the sequence too, as [differential] closes i.
	defer p.other.close()
	var log []string
	fail := func(format string, args ...any) string {
		return fmt.Sprintf("seed %v, after\n%v\n%v", seed, strings.Join(log, "\n"), fmt.Sprintf(format, args...))
	}
	for range n {
		if restarts && r.IntN(10) == 0 {
			empty := r.IntN(2) == 0
			log = append(log, fmt.Sprint("-- restart, with an empty cache: ", empty))
			if p.o.indexes(t) != "" {
				p.count(map[bool]string{true: "replay of indexes into an empty cache", false: "restart with indexes in the cache"}[empty])
			}
			i.restart(empty, options)
			if got, want := i.state(), p.o.state(t); got != want {
				return fail("after the restart the engine holds\n%v\nand SQLite\n%v", got, want)
			}
		}
		c := q.Next()
		log = append(log, fmt.Sprintf("%v -- with %#v", c.Src, c.Args))
		if failure := p.step(c); failure != "" {
			return fail("%v", failure)
		}
	}
	want := p.o.state(t)
	if got := i.state(); got != want {
		return fail("at the end the engine holds\n%v\nand SQLite\n%v", got, want)
	}
	if got := p.other.state(); got != want {
		return fail("at the end the other engine, which reads the rows the other way, holds\n%v\nand SQLite\n%v", got, want)
	}
	return ""
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
	// reads the rows another way: by a path that [otherPaths] gives where the engine of i reads as the
	// rules choose, and as the rules choose where it reads every row. See [pair.bothWays].
	other *instance
	// ways counts the calls of [pair.bothWays], which takes the paths of the other instance in turn.
	ways int
	// last is the engine's answer of the last call of [pair.step].
	last answer
	// otherOrdered is the answer of the other instance to the query of the call of [pair.bothWays], if
	// it is a select with ORDER BY or LIMIT that gives rows on both, which [pair.step] compares with
	// SQLite in order, or nil.
	otherOrdered *answer
	// rnd draws the candidates of [pair.everyPath], from a seed of the pair's own, so that the
	// sequence draws as it would without them.
	rnd *rand.Rand
	// inTx is set while the statements run in a transaction.
	inTx bool
	// allPaths is set for a pair whose [pair.everyPath] runs each WHERE by every candidate, and not by
	// [pathSample] of them.
	allPaths bool
	// wrote is set while the statements run in a write transaction in which a statement changed
	// rows.
	wrote bool
	// found holds the tables, folded, that a create table if not exists of the sequence found, and
	// that no drop table dropped since, for [pair.reach].
	found map[string]bool
	y     tally
}

// newPair of the instance i and SQLite, with the length limit of i, and a second instance of the
// engine of the kind of i, on a database in memory or on the core with the length limit of i.
func newPair(t testing.TB, i *instance, y tally) *pair {
	t.Helper()
	return newPairWith(t, i, y, engine.Options{})
}

// newPairWith is [newPair] with the options of the other instance on the core, options.
func newPairWith(t testing.TB, i *instance, y tally, options engine.Options) *pair {
	t.Helper()
	p := &pair{t: t, i: i, o: newOracle(t), empty: newOracle(t), found: map[string]bool{}, y: y, rnd: rand.New(rand.NewPCG(0, 0x9a7))}
	if i.memory != nil {
		p.other = newMemoryInstance(t)
	} else {
		p.other = newInstanceLimited(t, options, nil, i.limit)
		p.other.planted = i.planted
	}
	t.Cleanup(p.o.close)
	t.Cleanup(p.empty.close)
	if i.limit > 0 {
		p.o.lengthLimit(t, i.limit)
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
	s, err := stmt.Check(src)
	if err != nil || s == nil {
		args = nil
	}
	// A statement with a subquery, of slice 15, is refused before its values and its arguments until
	// the engine runs it, as [subqueried] says, so no rule of its values holds for it.
	if s == nil || !subqueried(s) {
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
	}
	if got.err == "not supported" && p.tooLongForCache(src, got.message) {
		p.y.excused(excusedCacheLimit)
		return ""
	}
	if got.err == "not supported" {
		// A statement of the generator and every statement that the pass supports are supported, but
		// for the refusals of their arguments and of the types of their values, which are checked
		// above, and a text that is not supported is one that SQLite accepts: SQLite prepares it on an
		// empty database without a syntax error. A statement with a subquery, of slice 15, is not
		// supported until the engine runs it, as [subqueried] says.
		if s, err := stmt.Check(src); c.Supported || err == nil && s != nil && !subqueried(s) {
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
	p.y.compared()
	inOrder := p.returnedInOrder(u, args)
	want := o.run(t, src, args...)
	if want.err == tooBig && got.err == "" {
		if unlimited, ok := p.returningTooBig(u, src, args); ok {
			// SQLite's rows without the limit, which the comparison below compares with the engine's.
			want = unlimited
			if want.String() == got.String() {
				p.y.excused(excusedReturningTooBig)
			}
		}
	}
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
	// The rows of a query with ORDER BY or LIMIT are compared as [ordered.differs] says, those of the
	// instance and those of the other instance, which [pair.bothWays] compared by their number alone.
	differs := got.String() != want.String()
	if sel, ok := u.(*stmt.Select); ok && c.Query && (sel.OrderBy != nil || sel.Limited()) && got.err == "" && want.err == "" {
		q := orderedOf(sel, args)
		if how := q.compare(t, o, got, want); how != "" {
			return fmt.Sprintf("%q with the arguments %v: %v", src, args, how)
		}
		if p.otherOrdered != nil {
			if how := q.compare(t, o, *p.otherOrdered, want); how != "" {
				return fmt.Sprintf("%q with the arguments %v: by the path of the other instance, %v", src, args, how)
			}
		}
		differs = false
	}
	if differs {
		gap := knownGap(src, got, want)
		switch {
		case startedTable(t, p.empty, src, got, want):
			p.y.excused(acceptedStartedTable)
		case want.err == tooBig && got.err == "" && c.Query && p.sortTooBig(src, args, got):
			p.y.excused(excusedSortTooBig)
		case got.err == tooBig && p.longCondition(src) && (want.err == "" || want.err == "20/20: datatype mismatch" && limitRefused(u, args)):
			// SQLite turns a blob literal that is too long into NULL, and then gives its error of an
			// argument of LIMIT that s4ql refuses after the length of the literal.
			p.y.excused(excusedLongCondition)
		case gap != "":
			p.y.excused(gap + knownGapSuffix)
		default:
			return fmt.Sprintf("%q with the arguments %v: the engine gives\n%v\nand SQLite\n%v", src, args, got, want)
		}
	}
	if sel, ok := u.(*stmt.Select); ok && c.Query && got.err == "" && want.err == "" {
		if failure := p.checkOrder(sel, args, got); failure != "" {
			return fmt.Sprintf("%q with the arguments %v: %v", src, args, failure)
		}
		if failure := p.checkAccess(sel, args, got); failure != "" {
			return fmt.Sprintf("%q with the arguments %v: %v", src, args, failure)
		}
	}
	if c.Query && got.err == "" && want.err == "" {
		if failure := p.checkReturned(u, got, inOrder); failure != "" {
			return fmt.Sprintf("%q with the arguments %v: %v", src, args, failure)
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

// sortTooBig reports whether the call of the query src, with the arguments args, whose answer of
// the engine is got, is the known difference of slice 13 under a length limit: a select with ORDER
// BY, which SQLite refuses with `string or blob too big` for the record of what it sorts of a row,
// and whose rows of s4ql are those that SQLite gives for it without the length limit, in order, as
// [ordered.compare] says. A select with a literal in a condition that is longer than the limit is
// not, as [pair.longCondition] says: the rule of the length of a literal refuses it in s4ql.
func (p *pair) sortTooBig(src string, args []any, got answer) bool {
	t, o := p.t, p.o
	t.Helper()
	s, _ := stmt.Check(src)
	sel, ok := s.(*stmt.Select)
	if !ok || sel.OrderBy == nil || p.i.limit == 0 || len(args) != sel.Parameters() || p.longCondition(src) {
		return false
	}
	want := o.unlimited(t, src, args...)
	return want.err == "" && orderedOf(sel, args).compare(t, o, got, want) == ""
}

// returningTooBig reports whether the call of src, the statement s, with the arguments args, which
// SQLite has just refused with `string or blob too big`, may be the known difference of slice 14
// under a lowered length limit, and gives SQLite's answer without the limit then: an insert, an
// update, or a delete whose RETURNING names a column twice, which SQLite refuses for the record of a
// row of RETURNING, longer than the row, and which SQLite runs without RETURNING under the limit, in
// a savepoint that is rolled back. The run without the limit changes SQLite's database, so the
// caller compares its answer with the engine's in place of the refusal.
func (p *pair) returningTooBig(s stmt.Statement, src string, args []any) (answer, bool) {
	t, o := p.t, p.o
	t.Helper()
	var r stmt.Returning
	switch s := s.(type) {
	case *stmt.Insert:
		r = s.Returning
	case *stmt.Update:
		r = s.Returning
	case *stmt.Delete:
		r = s.Returning
	}
	if !r.Present() || p.i.limit == 0 || len(args) != s.Parameters() || p.longCondition(src) {
		return answer{}, false
	}
	folds := map[string]bool{}
	twice := false
	for _, c := range r.Columns {
		twice = twice || folds[c.Fold()]
		folds[c.Fold()] = true
	}
	if !twice {
		return answer{}, false
	}
	is.Equal(t, "", o.run(t, "savepoint s4ql_returning").err)
	without := o.run(t, src[:r.Span.Start]+src[r.Span.End:], args...)
	is.Equal(t, "", o.run(t, "rollback to s4ql_returning").err)
	is.Equal(t, "", o.run(t, "release s4ql_returning").err)
	if without.err != "" {
		return answer{}, false
	}
	return o.unlimited(t, src, args...), true
}

// checkOrder of the rows got of the engine of the select s with the arguments args, which SQLite
// gives as well, by the access of the rows, with the direction that the engine drew, as
// [engine.Rows.Access] gives it: they must be in the order in which the access gives them, as
// [ordered.inOrderOf] says, which is also the order of the rows that tie under an ORDER BY. Where the
// direction shows in the rows, which it does where the rows in the other direction differ, it counts
// what the spec's rule "Unspecified row order varies" says varies: for a select without ORDER BY,
// whether the rows came in the order of the key of the access or in its reverse; for one whose rows
// are sorted, whether the rows that tie came in the order of the primary key or in its reverse; and
// for one whose order the access delivers, that the rows that tie came in the order of the key of the
// access. It gives what differs, or "".
func (p *pair) checkOrder(s *stmt.Select, args []any, got answer) string {
	t, o := p.t, p.o
	t.Helper()
	if got.access == nil || len(got.rows) < 2 || got.access.None || got.access.Point != nil {
		return ""
	}
	a := *got.access
	table, columns := o.columns(t, s.Table.Value)
	if columns == nil {
		return ""
	}
	ct := createTableOf(table, columns)
	cis, _ := o.tableIndexes(t, table, columns)
	key := accessKey(a, ct, cis)
	q := orderedOf(s, args)
	inOrder := q.inOrderOf(s, a, key, ct)
	want := o.unlimited(t, inOrder.src(), inOrder.args...)
	if want.err != "" {
		t.Fatalf("%v: SQLite gives %v", inOrder.src(), want.err)
	}
	if !slices.Equal(got.rows, want.rows) {
		return fmt.Sprintf("the engine gives the rows\n%v\nby %v, and SQLite, as %q,\n%v", got.rows, a, inOrder.src(), want.rows)
	}
	// other gives the rows of the access a, or of a in the other direction if turned, in the order of
	// its rows that tie if it sorts.
	other := func(a engine.Access, turned bool) []string {
		a.Backward = a.Backward != turned
		in := q.inOrderOf(s, a, key, ct)
		rows := o.unlimited(t, in.src(), in.args...)
		if rows.err != "" {
			t.Fatalf("%v: SQLite gives %v", in.src(), rows.err)
		}
		return rows.rows
	}
	switch {
	case s.OrderBy == nil:
		var turned []string
		if s.Limited() {
			turned = other(a, true)
		} else {
			turned = slices.Clone(want.rows)
			slices.Reverse(turned)
		}
		if !slices.Equal(turned, want.rows) {
			p.count("select without ORDER BY whose rows came in the " + map[bool]string{false: "order", true: "reverse of the order"}[a.Backward] + " of the key of its access")
		}
	case a.Sorted:
		if !slices.Equal(other(a, true), want.rows) {
			p.count("sorted select whose rows that tie came in the " + map[bool]string{false: "order", true: "reverse of the order"}[a.Backward] + " of the primary key")
		}
	case a.Delivered:
		sorted := a
		sorted.Delivered, sorted.Sorted = false, true
		if !slices.Equal(other(sorted, false), other(sorted, true)) {
			p.count("delivered select whose rows that tie came in the order of the key of its access")
		}
	}
	return ""
}

// returnedInOrder of the update or the delete s with RETURNING, with the arguments args as SQLite
// binds them, before SQLite runs it: the rows that the statement gives, in the order in which the
// engine changes them, which is the order of the primary key of the rows that its WHERE finds. They
// are the rows of a select of the columns of RETURNING with the WHERE of s, before the change, in
// the order of the key, where a column that an update sets is its value, which the update gives
// every row that it finds. A real of the SET is 0.0 for -0.0, as the table stores it. It gives nil
// for another statement, and where SQLite fails the select.
func (p *pair) returnedInOrder(s stmt.Statement, args []any) []string {
	t, o := p.t, p.o
	t.Helper()
	var (
		table stmt.Name
		where []stmt.Condition
		set   []stmt.Assignment
		r     stmt.Returning
	)
	switch s := s.(type) {
	case *stmt.Update:
		table, where, set, r = s.Table, s.Where, s.Set, s.Returning
	case *stmt.Delete:
		table, where, r = s.Table, s.Where, s.Returning
	default:
		return nil
	}
	if !r.Present() || len(args) != s.Parameters() {
		return nil
	}
	_, columns := o.columns(t, table.Value)
	if columns == nil {
		return nil
	}
	var names []string
	key := ""
	for _, c := range columns {
		if r.Columns == nil {
			names = append(names, c.Name)
		}
		if c.PrimaryKey {
			key = c.Name
		}
	}
	for _, c := range r.Columns {
		names = append(names, c.Value)
	}
	var list []string
	var bound []any
	for _, name := range names {
		term := stmttest.Quote(name)
		for _, a := range set {
			if a.Column.Fold() != fold(name) {
				continue
			}
			v := termArg(a.Value, args)
			term = "?"
			if _, ok := v.(float64); ok {
				term = "(? + 0.0)"
			}
			bound = append(bound, v)
		}
		list = append(list, term)
	}
	whole, whereArgs := stmttest.SelectText(&stmt.Select{Table: table, Where: where}, args, " order by "+stmttest.Quote(key))
	src := "select " + strings.Join(list, ", ") + strings.TrimPrefix(whole, "select *")
	rows := o.unlimited(t, src, append(bound, whereArgs...)...)
	if rows.err != "" {
		return nil
	}
	return rows.rows
}

// termArg is the value of the term, a literal or the argument of args for its parameter, as SQLite
// binds it.
func termArg(term stmt.Term, args []any) any {
	switch v := term.Value.(type) {
	case stmt.Integer:
		return int64(v)
	case stmt.Real:
		return float64(v)
	case stmt.Text:
		return string(v)
	case stmt.Blob:
		return []byte(v)
	case stmt.Null:
		return nil
	}
	return args[term.Parameter-1]
}

// checkReturned of the rows got of the engine of s, an update or a delete with RETURNING, which
// SQLite gives as well, by inOrder, the rows of [pair.returnedInOrder]: they must be inOrder, or its
// reverse where the engine drew backward, as [engine.Rows.Backward] says. Where the reverse differs,
// it counts what the spec's rule "Unspecified row order varies" says varies, by the kind of s, so
// that an engine whose rows of one kind never vary cannot hide behind those of the other: whether
// the rows came in the order of the key or in its reverse. It gives what differs, or "".
func (p *pair) checkReturned(s stmt.Statement, got answer, inOrder []string) string {
	if got.backward == nil || inOrder == nil || len(got.rows) < 2 {
		return ""
	}
	kind := "update"
	if _, ok := s.(*stmt.Delete); ok {
		kind = "delete"
	}
	want, turned := inOrder, slices.Clone(inOrder)
	slices.Reverse(turned)
	if *got.backward {
		want = turned
	}
	if !slices.Equal(got.rows, want) {
		return fmt.Sprintf("the engine gives the rows of RETURNING\n%v\nbackward: %v, and SQLite, in the order of the key,\n%v", got.rows, *got.backward, inOrder)
	}
	if !slices.Equal(turned, inOrder) {
		p.count(kind + " with RETURNING whose rows came in the " + map[bool]string{false: "order", true: "reverse of the order"}[*got.backward] + " of the key")
	}
	return ""
}

// checkAccess of the rows got of the engine of the select s with the arguments args, which SQLite
// gives as well: the access that the run reports, as [engine.Rows.Access] gives it, must be the
// model's choice, as [stmttest.ChosenAccess] gives it for the indexes of the table as SQLite lists
// them: the same candidate, the same pins and bounds, and delivered, in the same direction, or
// sorted alike. [checkChoice] checks the rules on the indexes that SQLite lists, and this the choice
// that the run made on the indexes that it read itself. An engine that forces a path is not checked.
// It gives what differs, or "".
func (p *pair) checkAccess(s *stmt.Select, args []any, got answer) string {
	t, o := p.t, p.o
	t.Helper()
	if got.access == nil || p.i.e == nil || p.i.e.Forced() {
		return ""
	}
	table, columns := o.columns(t, s.Table.Value)
	if columns == nil {
		return ""
	}
	_, indexes := o.tableIndexes(t, table, columns)
	want, ok := stmttest.ChosenAccess(columns, indexes, s, args)
	if !ok {
		return ""
	}
	if a := accessOf(*got.access); !a.Same(want) {
		return fmt.Sprintf("the run reads the rows by %v, and the model chooses %v", a, want)
	}
	p.count("select read by the access that the model chooses")
	return ""
}

// createDefaults are the kinds of the defaults of slice 11 of the create table s, each once, for a
// count of what a test reaches: "create table with a default of integer" (of real, of text, or of
// blob), "create table with DEFAULT NULL on a column that takes NULL", "create table with DEFAULT
// NULL on a column that does not take NULL", "create table with a default on the key", and "create
// table with a default of [stmttest.LongDefault] bytes or more".
func createDefaults(s *stmt.CreateTable) []string {
	seen := map[string]bool{}
	var kinds []string
	add := func(kind string) {
		if !seen[kind] {
			seen[kind] = true
			kinds = append(kinds, kind)
		}
	}
	for _, c := range s.Columns {
		switch v := c.Default.Value.(type) {
		case nil:
			continue
		case stmt.Null:
			add(map[bool]string{true: "create table with DEFAULT NULL on a column that takes NULL", false: "create table with DEFAULT NULL on a column that does not take NULL"}[!c.PrimaryKey && !c.NotNull])
		default:
			add("create table with a default of " + map[string]string{"stmt.Integer": "integer", "stmt.Real": "real", "stmt.Text": "text", "stmt.Blob": "blob"}[fmt.Sprintf("%T", v)])
		}
		if c.PrimaryKey {
			add("create table with a default on the key")
		}
		if c.Default.Span.End-c.Default.Span.Start >= stmttest.LongDefault {
			add("create table with a default of " + fmt.Sprint(stmttest.LongDefault) + " bytes or more")
		}
	}
	return kinds
}

// cacheStatementLength of the create table s: the length of its statement as the storage core keeps
// it in the cache under the length limit, without "main.", written again here apart from the core,
// so that the excuse of [pair.tooLongForCache] holds only where the cache's statement is longer than
// the limit: every name in double quotes with each double quote doubled, the declared type in lower
// case, PRIMARY KEY after the key, NOT NULL after every other column that does not take NULL, and
// each default in the core's canonical form, an integer in decimal, a real in the shortest exponent
// form and -0.0 as 0.0, text in single quotes with each doubled, a blob as x and its lower-case hex
// digits in single quotes, and null.
func cacheStatementLength(s *stmt.CreateTable) int {
	quote := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
	var b strings.Builder
	b.WriteString("create table " + quote(s.Table.Value) + " (")
	for i, c := range s.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quote(c.Name.Value) + " " + strings.ToLower(c.Type.String()))
		switch {
		case c.PrimaryKey:
			b.WriteString(" primary key")
		case c.NotNull:
			b.WriteString(" not null")
		}
		switch v := c.Default.Value.(type) {
		case nil:
		case stmt.Integer:
			b.WriteString(" default " + fmt.Sprint(int64(v)))
		case stmt.Real:
			f := float64(v)
			if f == 0 {
				f = 0
			}
			b.WriteString(" default " + strconv.FormatFloat(f, 'e', -1, 64))
		case stmt.Text:
			b.WriteString(" default '" + strings.ReplaceAll(string(v), "'", "''") + "'")
		case stmt.Blob:
			b.WriteString(" default x'" + hex.EncodeToString(v) + "'")
		case stmt.Null:
			b.WriteString(" default null")
		}
	}
	b.WriteString(") strict, without rowid")
	return b.Len()
}

// tooLongForCache reports whether message is the engine's refusal of the create table of the text
// src as one whose statement in the cache is longer than the length limit, where that statement, as
// [cacheStatementLength] measures it, is longer than the limit, and SQLite takes the text: SQLite
// runs it in a savepoint, which is rolled back.
func (p *pair) tooLongForCache(src, message string) bool {
	t := p.t
	t.Helper()
	s, _ := stmt.Check(src)
	c, ok := s.(*stmt.CreateTable)
	if !ok || p.i.limit == 0 || message != "s4ql: not supported: the table "+c.Table.Value+", whose statement in the cache is longer than the length limit" {
		return false
	}
	if cacheStatementLength(c) <= p.i.limit {
		return false
	}
	is.Equal(t, "", p.o.run(t, "savepoint s4ql_cache").err)
	defer func() {
		is.Equal(t, "", p.o.run(t, "rollback to s4ql_cache").err)
		is.Equal(t, "", p.o.run(t, "release s4ql_cache").err)
	}()
	return p.o.run(t, src).err == ""
}

// otherPaths are the paths that the other instance of a [pair] runs the calls by, in turn, where the
// instance reads as the rules choose: every row of the table, and the primary key, which scans with
// the key pinned where the rules get one row, each forward and backward, and then each index of the
// names, forward and backward. A path of an index that the table of a statement does not have reads
// as the rules choose.
func otherPaths(indexes ...string) []engine.Path {
	paths := []engine.Path{{Full: true, Forward: true}, {Full: true, Backward: true}, {Primary: true, Forward: true}, {Primary: true, Backward: true}}
	for _, name := range indexes {
		paths = append(paths, engine.Path{Index: name, Forward: true}, engine.Path{Index: name, Backward: true})
	}
	return paths
}

// candidate is a path that can serve a select, and the access that the engine reads by on it.
type candidate struct {
	path   engine.Path
	access engine.Access
}

// candidates of the select src, with the arguments args, on the table ct with the indexes cis, in
// the order in which they were created: every path that can serve it, each once for what it reads
// and its direction. They are every row, the primary key, and each index, each with every number of
// pinned columns that it can have, of which the most bounds the next column if the conditions bound
// it, and each forward and backward, and for a select with ORDER BY each also with a sort, as
// [engine.Path] has Sort.
func candidates(src string, ct *s4qlpb.CreateTable, cis []*s4qlpb.CreateIndex, args []*s4qlpb.Value) []candidate {
	var cs []candidate
	seen := map[string]bool{}
	add := func(p engine.Path) (engine.Access, bool) {
		a, ok := engine.PathAccessOf(src, ct, cis, args, p)
		if !ok {
			return a, false
		}
		if key := fmt.Sprint(a.String(), a.Index, a.Equal, a.Lower, a.LowerOpen, a.Upper, a.UpperOpen, a.Backward); !seen[key] {
			seen[key] = true
			cs = append(cs, candidate{path: p, access: a})
		}
		return a, true
	}
	names := []string{""}
	for _, ci := range cis {
		names = append(names, ci.GetIndex())
	}
	// A select with ORDER BY is also read by each path with a sort, where the key delivers the order.
	sorts := []bool{false}
	if s, _ := stmt.Check(src); s != nil {
		if sel, ok := s.(*stmt.Select); ok && sel.OrderBy != nil {
			sorts = append(sorts, true)
		}
	}
	for _, sort := range sorts {
		for _, backward := range []bool{false, true} {
			add(engine.Path{Full: true, Forward: !backward, Backward: backward, Sort: sort})
			for _, name := range names {
				p := engine.Path{Primary: name == "", Index: name, Forward: !backward, Backward: backward, Sort: sort}
				a, ok := add(p)
				if !ok {
					continue
				}
				for pins := range len(a.Equal) {
					p.Limited, p.Pins = true, pins
					add(p)
				}
			}
		}
	}
	return cs
}

// keyColumn of the key that a scan reads in the order of: the name of its column as the table stores
// it, and whether the key holds it in descending order.
type keyColumn struct {
	name       string
	descending bool
}

// accessKey of the access a on the table ct with the indexes cis: the key of the index of a, its columns and
// then those of the primary key that it does not hold, or the primary key.
func accessKey(a engine.Access, ct *s4qlpb.CreateTable, cis []*s4qlpb.CreateIndex) []keyColumn {
	var key []keyColumn
	held := map[uint32]bool{}
	for _, ci := range cis {
		if a.Index == "" || fold(ci.GetIndex()) != fold(a.Index) {
			continue
		}
		for _, c := range ci.GetColumns() {
			key = append(key, keyColumn{name: ct.GetColumns()[c.GetColumn()].GetName(), descending: c.GetDescending()})
			held[c.GetColumn()] = true
		}
	}
	for _, position := range ct.GetPrimaryKey() {
		if !held[position] {
			key = append(key, keyColumn{name: ct.GetColumns()[position].GetName()})
		}
	}
	return key
}

// orderOf the key: the ORDER BY that gives the rows in the order of the key, or in its reverse if
// backward.
func orderOf(key []keyColumn, backward bool) string {
	var terms []string
	for _, c := range key {
		term := stmttest.Quote(c.name)
		if c.descending != backward {
			term += " desc"
		}
		terms = append(terms, term)
	}
	return " order by " + strings.Join(terms, ", ")
}

// pathKinds of the candidate k, whose key is key, for the tally: whether it reads no row or every
// row, else by the primary key or by an index, with how many columns pinned and whether with a
// range, and whether backward, with a pin of NULL, with a range on a column that the key holds in
// descending order, by an index of a descending column, and by an index that holds the primary key.
func pathKinds(k candidate, key []keyColumn, ct *s4qlpb.CreateTable) []string {
	a := k.access
	var kinds []string
	if a.Backward {
		kinds = append(kinds, "path backward")
	} else {
		kinds = append(kinds, "path forward")
	}
	switch {
	case a.None:
		return append(kinds, "path that reads no row")
	case a.Full:
		return append(kinds, "path of every row")
	}
	by := "the primary key"
	if a.Index != "" {
		by = "an index"
	}
	ranged := ""
	if a.Lower != nil || a.Upper != nil {
		ranged = " and a range"
		if key[len(a.Equal)].descending {
			kinds = append(kinds, "path with a range on a descending column")
		}
	}
	kinds = append(kinds, fmt.Sprint("path of ", by, " with ", len(a.Equal), " pinned", ranged))
	if slices.ContainsFunc(a.Equal, (*s4qlpb.Value).HasNull) {
		kinds = append(kinds, "path with a pin of NULL")
	}
	if a.Index != "" {
		if slices.ContainsFunc(key, func(c keyColumn) bool { return c.descending }) {
			kinds = append(kinds, "path of an index of a descending column")
		}
		if len(key) > 0 && len(key) == len(ct.GetColumns()) {
			kinds = append(kinds, "path of an index that holds every column")
		}
	}
	return kinds
}

// pathSample is the number of candidates, as [candidates] gives them, that [pair.everyPath] runs for
// each select with WHERE, and for the WHERE of each update and delete, beside the access of the
// rules.
const pathSample = 4

// everyPath runs the WHERE of the statement s of the call c, a select with WHERE or ORDER BY or the
// select of the WHERE of an update or a delete, as [stmttest.SelectOf] gives it, on the other instance
// of the pair, before the call changes anything, with the ORDER BY and the LIMIT of a select: by the
// access of the rules, and by [pathSample] candidates of [candidates] at random, or by each if there
// are no more or the pair has allPaths. Each must give the rows that SQLite gives, as a multiset, or
// as [ordered.differs] says for a select with ORDER BY or LIMIT, and a candidate, whose path is
// forced, the rows in the order in which its access gives them, as [ordered.inOrderOf] says, which is
// the order of its key where nothing sorts. One candidate in four, and the access of the rules, run
// outside a transaction on the core, also go through [core.ReadTx.ScanPlan], and SQLite must search
// the index and the bounds that the engine asked for, as [wantPlan] says, and sort exactly where the
// access is sorted. A select that s4ql refuses, or SQLite, or that has an argument that no column
// holds, is left to the comparison of the call. The candidates that it runs are counted in the
// tally, by [pathKinds]. It gives what differs, or "".
func (p *pair) everyPath(s stmt.Statement, c stmttest.Call) string {
	t, o, other := p.t, p.o, p.other
	t.Helper()
	if s != nil && subqueried(s) {
		// The engine refuses a statement with a subquery, of slice 15, until it runs it.
		return ""
	}
	var sel *stmt.Select
	args := c.Args
	switch s := s.(type) {
	case *stmt.Select:
		sel = s
	case *stmt.Update, *stmt.Delete:
		sel, args = stmttest.SelectOf(s, c.Args)
	}
	if sel == nil || sel.Where == nil && sel.OrderBy == nil || len(args) != sel.Parameters() || slices.ContainsFunc(args, func(a any) bool { return refusal(a) != "" }) || p.longCondition(c.Src) {
		return ""
	}
	table, columns := o.columns(t, sel.Table.Value)
	if columns == nil {
		return ""
	}
	q := orderedOf(sel, oracleArgs(args))
	src, bound := q.src(), q.args
	want := o.run(t, src, bound...)
	if want.err != "" {
		return ""
	}
	vals := values(bound)
	defer other.e.SetPath(engine.Path{})
	other.e.SetPath(engine.Path{})
	rules := other.queryValues(src, vals)
	if rules.err == "not supported" {
		// s4ql's refusal of a value of another type than its column, which the comparison of the call
		// checks against s4ql's rule.
		return ""
	}
	// differs says how the answer got differs from SQLite's, or "".
	differs := func(got answer) string {
		switch {
		case !q.plain():
			return q.compare(t, o, got, want)
		case got.String() != want.String():
			return fmt.Sprintf("the engine gives\n%v\nand SQLite\n%v", got, want)
		}
		return ""
	}
	if how := differs(rules); how != "" {
		return fmt.Sprintf("%q with %#v: the select, as %q with %#v, as the rules choose: %v", c.Src, c.Args, src, bound, how)
	}
	ct := createTableOf(table, columns)
	cis, _ := o.tableIndexes(t, table, columns)
	if a, ok := engine.AccessOf(src, ct, cis, vals); ok && p.rnd.IntN(4) == 0 {
		if failure := p.checkPlan(src, table, a, ct, cis); failure != "" {
			return failure
		}
	}
	cs := candidates(src, ct, cis, vals)
	if sel.Where != nil {
		p.count("WHERE run by every path")
		if len(cis) > 0 {
			p.count("WHERE run by every path on a table with an index")
		}
	}
	if sel.OrderBy != nil {
		p.count("ORDER BY run by every path")
	}
	if sel.Limited() {
		p.count("LIMIT run by every path")
	}
	// A WHERE with a value on the left, which src writes as the text does.
	valueFirst := slices.ContainsFunc(sel.Where, func(c stmt.Condition) bool { return c.ValueFirst })
	if valueFirst {
		p.count("WHERE with a value on the left run by every path")
		if len(cis) > 0 {
			p.count("WHERE with a value on the left run by every path on a table with an index")
		}
	}
	if !p.allPaths && len(cs) > pathSample {
		for j := range pathSample {
			k := j + p.rnd.IntN(len(cs)-j)
			cs[j], cs[k] = cs[k], cs[j]
		}
		cs = cs[:pathSample]
		if sel.Where != nil {
			p.count("WHERE run by a sample of its paths")
		}
	}
	for _, k := range cs {
		other.e.SetPath(k.path)
		got := other.queryValues(src, vals)
		if how := differs(got); how != "" {
			return fmt.Sprintf("%q with %#v: the select, as %q with %#v, by the path %+v, which reads by %v: %v", c.Src, c.Args, src, bound, k.path, k.access, how)
		}
		key := accessKey(k.access, ct, cis)
		inOrder := q.inOrderOf(sel, k.access, key, ct)
		in := o.unlimited(t, inOrder.src(), inOrder.args...)
		if in.err != "" {
			t.Fatalf("%v: SQLite gives %v", inOrder.src(), in.err)
		}
		if !k.access.None && !slices.Equal(got.rows, in.rows) {
			return fmt.Sprintf("%q with %#v: the select gives the rows\n%v\nby the path %+v, which reads by %v, and SQLite, as %q,\n%v", c.Src, c.Args, got.rows, k.path, k.access, inOrder.src(), in.rows)
		}
		if k.access.Delivered {
			p.count("path that delivers the order")
		}
		if k.access.Sorted {
			p.count("path that sorts")
		}
		for _, kind := range pathKinds(k, key, ct) {
			p.count(kind)
			if valueFirst && strings.HasSuffix(kind, " and a range") {
				p.count(kind + " of a WHERE with a value on the left")
			}
		}
		if p.rnd.IntN(4) == 0 {
			if failure := p.checkPlan(src, table, k.access, ct, cis); failure != "" {
				return failure
			}
		}
	}
	return ""
}

// checkPlan of the select src on the table of the name, which reads by the access a, on the table ct
// with the indexes cis: on the other instance of the pair, outside a transaction and on the core, the
// plan of SQLite for the scan of a, as [core.ReadTx.ScanPlan] gives it, must be the row that
// [wantPlan] gives, and then SQLite's sort exactly where a is sorted. An access that reads no row, or
// one row by a get, has no scan. It gives what differs, or "".
func (p *pair) checkPlan(src, table string, a engine.Access, ct *s4qlpb.CreateTable, cis []*s4qlpb.CreateIndex) string {
	other := p.other
	if other.d == nil || p.inTx || a.None || a.Point != nil {
		return ""
	}
	sql, plan := scanPlan(p.t, other, table, a)
	want := wantPlans(a, ct, cis)
	if !slices.Equal(plan, want) {
		return fmt.Sprintf("%q: the cache runs %q for the access %v, and SQLite's plan for it is %q, where it must be %q", src, sql, a, plan, want)
	}
	p.count("plan of the cache checked")
	return ""
}

// wantPlans are the rows of SQLite's plan of the statement of the cache for the scan of the access a
// on the table ct with the indexes cis: the row of [wantPlan], and then SQLite's sort exactly where a
// is sorted.
func wantPlans(a engine.Access, ct *s4qlpb.CreateTable, cis []*s4qlpb.CreateIndex) []string {
	plan := []string{wantPlan(a, ct, cis)}
	if a.Sorted {
		plan = append(plan, "USE TEMP B-TREE FOR ORDER BY")
	}
	return plan
}

// scanPlan of the scan of the access a of the table of the name in the instance i, outside a
// transaction: the statement that the cache runs, and SQLite's plan for it, as
// [core.ReadTx.ScanPlan] gives them.
func scanPlan(t testing.TB, i *instance, table string, a engine.Access) (string, []string) {
	t.Helper()
	ctx := t.Context()
	rd, err := i.d.Reader(ctx)
	is.NotError(t, err)
	defer func() { is.NotError(t, rd.Close(ctx)) }()
	r, err := rd.Read(ctx)
	is.NotError(t, err)
	defer func() { is.NotError(t, r.Close(ctx)) }()
	sql, plan, err := r.ScanPlan(ctx, table, a.Scan())
	is.NotError(t, err)
	return sql, plan
}

// wantPlan is the one row of SQLite's plan of the statement of the cache for the scan of the access
// a on the table ct with the indexes cis: a search of the index of a, or of the primary key, with a
// constraint for each pinned column and for each bound of the next, or a scan of the index or of the
// table where a pins and bounds nothing, and no sort, since the statement orders by the key that the
// index holds. An index whose key holds every column of the table covers it.
func wantPlan(a engine.Access, ct *s4qlpb.CreateTable, cis []*s4qlpb.CreateIndex) string {
	key := accessKey(a, ct, cis)
	var constraints []string
	for i := range a.Equal {
		constraints = append(constraints, key[i].name+"=?")
	}
	if a.Lower != nil {
		constraints = append(constraints, key[len(a.Equal)].name+">?")
	}
	if a.Upper != nil {
		constraints = append(constraints, key[len(a.Equal)].name+"<?")
	}
	by := "PRIMARY KEY"
	if a.Index != "" {
		by = "INDEX " + a.Index
		if len(key) == len(ct.GetColumns()) {
			by = "COVERING " + by
		}
	}
	switch {
	case len(constraints) > 0:
		return fmt.Sprint("SEARCH main.", ct.GetTable(), " USING ", by, " (", strings.Join(constraints, " AND "), ")")
	case a.Index != "":
		return fmt.Sprint("SCAN main.", ct.GetTable(), " USING ", by)
	}
	return "SCAN main." + ct.GetTable()
}

// tableIndexes of the table of the name and the columns in SQLite's database, in the order in which
// they were created: as the engine's definitions, and as those of [stmttest.ChosenAccess].
func (o *oracle) tableIndexes(t testing.TB, table string, columns []stmttest.TableColumn) ([]*s4qlpb.CreateIndex, []stmttest.TableIndex) {
	t.Helper()
	list := o.run(t, "select name from sqlite_schema where type = 'index' and tbl_name = ? and sql is not null order by rowid", table)
	is.Equal(t, "", list.err)
	var cis []*s4qlpb.CreateIndex
	var model []stmttest.TableIndex
	for _, quoted := range list.rows {
		name := unquoted(t, quoted)
		xinfo := o.run(t, `select cid, "desc" from pragma_index_xinfo(?) where key = 1 order by seqno`, name)
		is.Equal(t, "", xinfo.err)
		ci := s4qlpb.CreateIndex_builder{Index: new(name), Table: new(table)}
		ix := stmttest.TableIndex{Name: name}
		for _, row := range xinfo.rows {
			cid, desc, _ := strings.Cut(row, ",")
			position, err := strconv.Atoi(cid)
			is.NotError(t, err)
			column := s4qlpb.IndexColumn_builder{Column: new(uint32(position))}
			if desc == "1" {
				column.Descending = new(true)
			}
			ci.Columns = append(ci.Columns, column.Build())
			ix.Columns = append(ix.Columns, stmttest.IndexKeyColumn{Position: position, Descending: desc == "1"})
		}
		cis = append(cis, ci.Build())
		model = append(model, ix)
	}
	return cis, model
}

// indexNames of the table of the select, the update, or the delete s, in SQLite's database, in the
// order in which they were created, or nil for another statement.
func (p *pair) indexNames(s stmt.Statement) []string {
	var table stmt.Name
	switch s := s.(type) {
	case *stmt.Select:
		table = s.Table
	case *stmt.Update:
		table = s.Table
	case *stmt.Delete:
		table = s.Table
	default:
		return nil
	}
	name, columns := p.o.columns(p.t, table.Value)
	if columns == nil {
		return nil
	}
	cis, _ := p.o.tableIndexes(p.t, name, columns)
	var names []string
	for _, ci := range cis {
		names = append(names, ci.GetIndex())
	}
	return names
}

// bothWays runs the call c, whose answer of the engine of the instance is got, on the other instance
// of the pair, through the same method, by the next of the paths of [otherPaths] for the indexes of
// the table of c, as [engine.Engine.SetPath] forces it, where the engine of the instance reads the
// rows as the rules choose, and as the rules choose where it reads every row, as an engine of
// -scanall does. Before that, for a WHERE or an ORDER BY, it runs [pair.everyPath]. The rows of the two are compared
// as multisets, as every two answers are: each query of the instance reads in a direction that its
// engine draws. Those of a query with LIMIT are compared by their number, since which rows a LIMIT
// keeps depends on the path, and [pair.step] compares those of a query with ORDER BY or LIMIT of each
// instance with SQLite. The two must give the same answer, message, and count of changes, and after an update
// or a delete with WHERE the same rows of its table, as a select of * gives them on each, so that each
// test that compares the instance with SQLite compares the ways of reading rows, of a select and of
// the rows that an update or a delete changes, also in transactions. The other instance runs every
// call, so that it holds what the instance holds. A second instance, and not a second run on the
// instance, because an update or a delete changes what a second run would find. bothWays counts a
// select with WHERE in the tally under "reached: read both ways", and an update or a delete with
// WHERE under "reached: changed both ways", each also with " in a read-only transaction" in one, and
// gives what differs, or "".
func (p *pair) bothWays(c stmttest.Call, got answer) string {
	i, other := p.i, p.other
	byKey := !i.e.ScansAll()
	s, _ := stmt.Check(c.Src)
	if failure := p.everyPath(s, c); failure != "" {
		return failure
	}
	path := engine.Path{}
	if byKey {
		paths := otherPaths(p.indexNames(s)...)
		path = paths[p.ways%len(paths)]
		p.ways++
	}
	other.e.SetPath(path)
	var all answer
	if c.Query {
		all = other.queryValues(c.Src, values(c.Args))
	} else {
		all = other.execValues(c.Src, values(c.Args))
	}
	changes, allChanges := i.changes, other.changes
	// Which rows a LIMIT keeps, of those that SQLite leaves unordered, depends on the path, and so does
	// the order of rows that tie, so the rows of a select with ORDER BY or LIMIT are compared by their
	// number here, and each answer with SQLite in [pair.step].
	same := all.String() == got.String()
	p.otherOrdered = nil
	if sel, ok := s.(*stmt.Select); ok && c.Query && (sel.OrderBy != nil || sel.Limited()) && all.err == "" && got.err == "" {
		same = all.columns == got.columns && len(all.rows) == len(got.rows)
		otherAnswer := all
		p.otherOrdered = &otherAnswer
	}
	if !byKey {
		// The instance read every row, as -scanall makes it, and the other instance by the key.
		got, all, changes, allChanges = all, got, allChanges, changes
	}
	if !same || all.message != got.message || allChanges != changes {
		return fmt.Sprintf("%q with %#v: the engine gives\n%v (%q)\nas the rules choose, and\n%v (%q)\nby the path %+v, with %v changes as the rules choose and %v by the path", c.Src, c.Args, got, got.message, all, all.message, path, changes, allChanges)
	}
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

// count the kind under "reached: " in the tally of the pair, if it has one.
func (p *pair) count(kind string) {
	if p.y != nil {
		p.y["reached: "+kind]++
	}
}

// reach counts in the tally what the call c, whose answer of the engine is got, reaches, under
// "reached: ", on the database of SQLite before the call, as [reach] says.
func (p *pair) reach(c stmttest.Call, got answer) {
	p.t.Helper()
	if p.y != nil {
		found := false
		reach(p.t, p.o, c, got, p.i.changes, func(what string) {
			p.y["reached: "+what]++
			found = found || what == "create table if not exists did nothing: a table of the name exists"
			// The statements of slice 10 count again by the transaction that they ran in.
			if p.inTx && slices.ContainsFunc([]string{"create table if not exists ", "drop table if exists ", "create index if not exists ", "drop index if exists "}, func(k string) bool { return strings.HasPrefix(what, k) }) {
				p.y["reached: "+what+map[bool]string{true: " in a read-only transaction", false: " in a write transaction"}[p.readOnly]]++
			}
			// So do the defaults of slice 11, the statements with a value on the left of slice 12, and
			// those with RETURNING of slice 14.
			if p.inTx && (strings.Contains(what, " default") || strings.Contains(what, "DEFAULT NULL") || strings.Contains(what, "with a value on the left ") || strings.Contains(what, " with RETURNING ")) {
				p.y["reached: "+what+map[bool]string{true: " in a read-only transaction", false: " in a write transaction"}[p.readOnly]]++
			}
		})
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
		// A value on the left, of slice 12, on a table that a create table if not exists, of slice 10,
		// found: a meeting of two areas that only the mixed profile has on.
		switch s := s.(type) {
		case *stmt.CreateTable:
			if found {
				p.found[s.Table.Fold()] = true
			}
		case *stmt.DropTable:
			if got.err == "" {
				delete(p.found, s.Table.Fold())
			}
		case *stmt.Select, *stmt.Update, *stmt.Delete:
			if table, where := tableWhere(s); p.found[table.Fold()] && slices.ContainsFunc(where, func(c stmt.Condition) bool { return c.ValueFirst }) {
				p.y["reached: value on the left on a table that a create table if not exists found"]++
			}
		}
		if sel, ok := s.(*stmt.Select); ok && sel.OrderBy != nil && c.Query && got.err == "" && len(got.rows) > 0 && p.wrote {
			p.y["reached: ORDER BY that gives rows in a write transaction after a change of rows"]++
		}
	}
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
	if s != nil && subqueried(s) {
		// The engine refuses a statement with a subquery, of slice 15, until it runs it, and the kinds
		// of what a statement reaches read no subquery.
		return
	}
	if reachExists(t, o, s, c, got, count) {
		return
	}
	reachReturning(t, o, s, c, got, outcome, count)
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
		for _, what := range createDefaults(s) {
			count(what + " " + outcome)
		}
	case *stmt.Update, *stmt.Delete:
		reachChange(t, o, s, c, got, outcome, changes, count)
	case *stmt.CreateIndex, *stmt.DropIndex:
		reachIndex(t, o, s, c, got, count)
	case *stmt.Select:
		table, columns := o.columns(t, s.Table.Value)
		if columns == nil {
			return
		}
		reachWhere(t, o, s, c, got, outcome, table, columns, count)
		reachOrder(t, o, s, c, got, outcome, table, columns, count)
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
	reachValueFirst(s, args, c, got, "select", outcome, count)
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
	if s.OrderBy != nil || limitRule(s, args) != "" {
		// The rule of the key chooses an access for the conditions alone, and an ORDER BY takes part in
		// the choice, which [reachOrder] checks against the model. An argument of LIMIT that s4ql refuses
		// leaves the select without an access.
		return
	}
	// The plan reads the rows by the key as the rule says, which the comparison of the rows cannot see
	// where it reads more rows than it needs.
	if want := stmttest.KeyAccess(columns, s, args); want != "" && got.err != tooBig {
		want = strings.TrimSuffix(want, " that hold no key")
		var access string
		if a, ok := engine.AccessOf(c.Src, createTableOf(table, columns), nil, values(c.Args)); ok {
			access = a.String()
		}
		if access != want {
			t.Errorf("%q with %#v: the plan reads %q, and the rule %q", c.Src, c.Args, access, want)
		}
	}
	if got.err != tooBig {
		checkChoice(t, o, s, args, c, table, columns, count)
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

// reachOrder counts what the select s with ORDER BY or LIMIT, of slice 13, of the call c, whose answer
// of the engine is got and whose outcome in the engine is outcome, reaches on the database of o,
// where its table has the name table and the columns, as [reach] says: a select with arguments that
// s4ql refuses, for their number or for one that no column holds, and one with an argument of LIMIT
// that s4ql refuses, by its kind; a LIMIT by an argument; a column of ORDER BY that the table does
// not have; the ORDER BY by its form, by the type and the NULL of each of its columns, by its
// directions, with a column that the WHERE pins, and without WHERE; for a query that succeeds, its
// LIMIT by what it keeps of the rows of the select without it, 0, all, more than all, or a part, and
// a part that cuts a group of rows that tie under the ORDER BY; and for one that SQLite and s4ql take,
// the choice of its access against the model, as [checkChoice] checks it, and an access that delivers
// the order with a LIMIT where without the LIMIT the model would choose a range, which is the shape
// of goqite's receive with goqite's index beside one on the queue and the timeout.
func reachOrder(t testing.TB, o *oracle, s *stmt.Select, c stmttest.Call, got answer, outcome, table string, columns []stmttest.TableColumn, count func(string)) {
	t.Helper()
	if s.OrderBy == nil && !s.Limited() {
		return
	}
	args := oracleArgs(c.Args)
	if len(args) != s.Parameters() || slices.ContainsFunc(args, func(a any) bool { return refusal(a) != "" }) {
		count("ORDER BY or LIMIT with arguments that s4ql refuses " + outcome)
		return
	}
	if rule := limitRule(s, args); rule != "" {
		count("LIMIT with an argument that is " + limitKind(args[s.Limit.Parameter-1]) + " " + outcome)
		return
	}
	if s.Limit.Parameter > 0 {
		count("LIMIT by an argument " + outcome)
	}
	for _, oc := range s.OrderBy {
		if stmttest.ColumnOf(columns, oc.Name) < 0 {
			count("ORDER BY of a column that the table does not have " + outcome)
			return
		}
	}
	q := orderedOf(s, args)
	if s.OrderBy != nil {
		count(fmt.Sprint("ORDER BY of ", len(s.OrderBy), " columns ", outcome))
		if s.Where == nil {
			count("ORDER BY without WHERE " + outcome)
		}
		descending := 0
		for _, oc := range s.OrderBy {
			column := columns[stmttest.ColumnOf(columns, oc.Name)]
			count("ORDER BY of a column of type " + column.Type + " " + outcome)
			if column.Nullable {
				count("ORDER BY of a column that takes NULL " + outcome)
			}
			if oc.Descending {
				descending++
			}
			if slices.ContainsFunc(s.Where, func(w stmt.Condition) bool {
				return w.Column.Fold() == oc.Name.Fold() && (w.Operator == stmt.OpEqual || w.Operator == stmt.OpIsNull && column.Nullable)
			}) {
				count("ORDER BY of a column that the WHERE pins " + outcome)
			}
		}
		switch descending {
		case 0:
			count("ORDER BY ascending " + outcome)
		case len(s.OrderBy):
			count("ORDER BY descending " + outcome)
		default:
			count("ORDER BY of mixed directions " + outcome)
		}
	}
	if s.OrderBy != nil && got.err != tooBig {
		checkChoice(t, o, s, args, c, table, columns, count)
	}
	if s.OrderBy != nil && s.Where != nil && got.err != tooBig {
		_, indexes := o.tableIndexes(t, table, columns)
		unlimited := *s
		unlimited.Limit = stmt.Term{}
		if with, ok := stmttest.ChosenAccess(columns, indexes, s, args); ok && s.Limited() && with.Delivered && with.Index != "" {
			if without, ok := stmttest.ChosenAccess(columns, indexes, &unlimited, slices.Clip(args[:len(args)-map[bool]int{true: 1, false: 0}[s.Limit.Parameter > 0]])); ok && !without.Same(with) && (without.Lower != nil || without.Upper != nil) && len(without.Pins) == len(with.Pins) {
				count("ORDER BY with LIMIT read by an index that delivers the order beside one that narrows a range")
				if without.Index != "" && len(with.Pins) > 0 {
					count("ORDER BY with LIMIT read by an index that delivers the order with a pin beside an index that narrows a range")
				}
			}
		}
	}
	if s.OrderBy != nil && got.err != tooBig {
		reachKeyShapes(t, o, s, args, c, got, columns, count)
	}
	if !c.Query || got.err != "" {
		return
	}
	if s.OrderBy != nil {
		var nulls []string
		for _, term := range orderTerms(s.OrderBy) {
			nulls = append(nulls, strings.TrimSuffix(term, " desc")+" is null")
		}
		src := "select count(*) from " + q.table + " where (" + strings.Join(nulls, " or ") + ")"
		if q.where != "" {
			src += " and " + q.where
		}
		if n := o.unlimited(t, src, q.unlimited().args...); n.err == "" && n.rows[0] != "0" {
			count("ORDER BY over rows with NULL in its columns")
		}
	}
	if s.Limited() {
		limit := int64(0)
		if s.Limit.Parameter > 0 {
			limit = args[s.Limit.Parameter-1].(int64)
		} else {
			limit = int64(s.Limit.Value.(stmt.Integer))
		}
		all := q.unlimited()
		ranked := "select "
		if s.OrderBy != nil {
			ranked += "dense_rank() over (order by " + all.by + ")"
		} else {
			ranked += "1"
		}
		ranked += " from " + all.table
		if all.where != "" {
			ranked += " where " + all.where
		}
		if all.by != "" {
			ranked += " order by " + all.by
		}
		ranks := o.unlimited(t, ranked, all.args...)
		if ranks.err != "" {
			t.Fatalf("%v: SQLite gives %v", ranked, ranks.err)
		}
		n := int64(len(ranks.rows))
		switch {
		case limit == 0:
			count("LIMIT 0")
		case n == 0:
		case limit > n:
			count("LIMIT above the number of rows")
		case limit == n:
			count("LIMIT of the number of rows")
		default:
			count("LIMIT that cuts")
			if s.OrderBy != nil && ranks.rows[limit-1] == ranks.rows[limit] {
				count("LIMIT that cuts a group of rows that tie")
			}
		}
	}
}

// reachKeyShapes counts the shapes of the ORDER BY of the select s, with the arguments args, of the
// call c, whose answer of the engine is got, against the keys of the indexes of its table, of the
// columns in o, where a column that the WHERE pins, with = or with IS NULL on a column that takes
// NULL, is taken out of the ORDER BY and of each key, as the rules take it out:
//
//   - "ORDER BY delivered around a pinned column", where the model's access delivers the order by a
//     key of which the WHERE pins a column after one that it does not pin, and before one that the
//     ORDER BY names;
//   - for a query that gives two rows or more, "ORDER BY of the key of an index with a gap, over rows
//     that the gap orders", where the ORDER BY names the first column of the key of an index and
//     then later ones, in the directions of the key or all turned, but leaves out a column between
//     them, and the rows of the select in the order of that key, up to the last column that the
//     ORDER BY names, differ in the columns of the ORDER BY from SQLite's rows for the ORDER BY: so
//     that a key that took the ORDER BY for its own order would give the rows in another order.
func reachKeyShapes(t testing.TB, o *oracle, s *stmt.Select, args []any, c stmttest.Call, got answer, columns []stmttest.TableColumn, count func(string)) {
	t.Helper()
	table, _ := o.columns(t, s.Table.Value)
	_, indexes := o.tableIndexes(t, table, columns)
	pinned := map[int]bool{}
	for _, w := range s.Where {
		if at := stmttest.ColumnOf(columns, w.Column); at >= 0 && (w.Operator == stmt.OpEqual || w.Operator == stmt.OpIsNull && columns[at].Nullable) {
			pinned[at] = true
		}
	}
	unpinned := func(key []stmttest.IndexKeyColumn) []stmttest.IndexKeyColumn {
		return slices.DeleteFunc(slices.Clone(key), func(k stmttest.IndexKeyColumn) bool { return pinned[k.Position] })
	}
	var by []stmttest.IndexKeyColumn
	for _, oc := range s.OrderBy {
		by = append(by, stmttest.IndexKeyColumn{Position: stmttest.ColumnOf(columns, oc.Name), Descending: oc.Descending})
	}
	by = unpinned(by)
	if len(by) == 0 {
		return
	}
	primary := slices.IndexFunc(columns, func(c stmttest.TableColumn) bool { return c.PrimaryKey })
	keyOf := func(ix stmttest.TableIndex) []stmttest.IndexKeyColumn {
		key := slices.Clone(ix.Columns)
		if !slices.ContainsFunc(key, func(k stmttest.IndexKeyColumn) bool { return k.Position == primary }) {
			key = append(key, stmttest.IndexKeyColumn{Position: primary})
		}
		return key
	}
	if with, ok := stmttest.ChosenAccess(columns, indexes, s, args); ok && with.Delivered && with.Index != "" {
		for _, ix := range indexes {
			if !strings.EqualFold(ix.Name, with.Index) {
				continue
			}
			key := keyOf(ix)
			last := -1
			for i, k := range key {
				if slices.ContainsFunc(by, func(b stmttest.IndexKeyColumn) bool { return b.Position == k.Position }) {
					last = i
				}
			}
			first := slices.IndexFunc(key, func(k stmttest.IndexKeyColumn) bool { return !pinned[k.Position] })
			if first >= 0 && slices.ContainsFunc(key[first+1:max(last, first+1)], func(k stmttest.IndexKeyColumn) bool { return pinned[k.Position] }) {
				count("ORDER BY delivered around a pinned column")
			}
			break
		}
	}
	if !c.Query || got.err != "" || len(got.rows) < 2 {
		return
	}
	for _, ix := range indexes {
		key := unpinned(keyOf(ix))
		if len(key) == 0 || key[0].Position != by[0].Position {
			continue
		}
		turned := key[0].Descending != by[0].Descending
		at, gap, ok := 0, false, true
		for _, b := range by {
			j := slices.IndexFunc(key[at:], func(k stmttest.IndexKeyColumn) bool { return k.Position == b.Position })
			if j < 0 || key[at+j].Descending != (b.Descending != turned) {
				ok = false
				break
			}
			gap = gap || j > 0
			at += j + 1
		}
		if !ok || !gap {
			continue
		}
		var fill []stmt.OrderColumn
		for _, k := range key[:at] {
			fill = append(fill, stmt.OrderColumn{Name: stmt.Name{Value: columns[k.Position].Name}, Descending: k.Descending != turned})
		}
		q := orderedOf(s, args).unlimited()
		var names []string
		for _, oc := range s.OrderBy {
			names = append(names, stmttest.Quote(oc.Name.Value))
		}
		q.columns = strings.Join(names, ", ")
		want := o.unlimited(t, q.src(), q.args...)
		q.by = strings.Join(orderTerms(fill), ", ")
		keyed := o.unlimited(t, q.src(), q.args...)
		if want.err == "" && keyed.err == "" && !slices.Equal(want.rows, keyed.rows) {
			count("ORDER BY of the key of an index with a gap, over rows that the gap orders")
			return
		}
	}
}

// reachValueFirst counts for the select, the update, or the delete of the call c, of the kind, whose
// WHERE is that of the select s with the arguments args, and whose outcome in the engine is outcome,
// each case of [stmttest.ValueFirstCases], the comparisons with the value on the left, of slice 12,
// by their operator as the text writes it, and those of them with NULL: after the kind with the
// outcome, and by itself with the outcome of any kind, where "ok" is "finds rows" or "finds no row",
// by the rows of a query and by the count of an update or a delete, whose outcome says so already.
// A select that is run as no query has no rows to tell, and keeps "ok".
func reachValueFirst(s *stmt.Select, args []any, c stmttest.Call, got answer, kind, outcome string, count func(string)) {
	ofAnyKind := outcome
	switch {
	case outcome == "ok" && kind == "select" && c.Query:
		outcome = map[bool]string{true: "ok, no row found", false: "ok, rows found"}[len(got.rows) == 0]
		ofAnyKind = map[bool]string{true: "finds no row", false: "finds rows"}[len(got.rows) == 0]
	case outcome == "ok, no row found":
		ofAnyKind = "finds no row"
	case outcome == "ok, rows changed":
		ofAnyKind = "finds rows"
	}
	cases := stmttest.ValueFirstCases(s, args)
	for _, what := range cases {
		count(what + " " + ofAnyKind)
		count(kind + " with a " + what + " " + outcome)
	}
	if cases != nil {
		count("value on the left " + ofAnyKind)
		count(kind + " with a value on the left " + outcome)
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
	if sel, selArgs := stmttest.SelectOf(s, args); sel != nil {
		reachValueFirst(sel, selArgs, c, got, kind, outcome, count)
	} else {
		reachValueFirst(&stmt.Select{Where: where}, nil, c, got, kind, outcome, count)
	}
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
			var access string
			if a, ok := engine.AccessOf(c.Src, createTableOf(table, columns), nil, values(c.Args)); ok {
				access = a.String()
			}
			if access != want {
				t.Errorf("%q with %#v: the scan of the rows reads %q, and the rule %q", c.Src, c.Args, access, want)
			}
		}
		if got.err != tooBig {
			choose := count
			if hasReturning(c.Src) {
				// The choice of an update or a delete with RETURNING is checked as without it.
				choose = func(what string) {
					count(what)
					if strings.HasPrefix(what, "chosen ") && !strings.HasSuffix(what, " by a value on the left") {
						count(kind + " with RETURNING read by the access that the model chooses")
					}
				}
			}
			checkChoice(t, o, sel, selArgs, c, table, columns, choose)
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

// reachIndex counts what the create index or the drop index s of the call c, whose answer of the
// engine is got, reaches on the database of o, before the call: its kind and its outcome, and for a
// create index that succeeds, the number of its columns, and whether it holds the primary key, a
// descending column, a column that takes NULL, and each type of column. For a call without
// arguments, it checks the outcome against [stmttest.IndexRule], a second statement of SQLite's
// errors of the names, which comes before SQLite's errors of a read-only transaction and of the
// length limit, which the rule does not know.
func reachIndex(t testing.TB, o *oracle, s stmt.Statement, c stmttest.Call, got answer, count func(string)) {
	t.Helper()
	if len(c.Args) > 0 {
		return
	}
	tables, indexes := o.schema(t)
	rule := stmttest.IndexRule(tables, indexes, s)
	kind := "drop index"
	if _, ok := s.(*stmt.CreateIndex); ok {
		kind = "create index"
	}
	switch {
	case rule == "" && (strings.Contains(got.err, "attempt to write a readonly database") || got.err == tooBig || got.err == "not supported"):
		count(kind + " " + messageKind(got.err))
		return
	case rule == "" && got.err != "":
		t.Errorf("%q: the engine gives %v, and the rule nothing", c.Src, got)
		return
	case rule != "" && !strings.HasSuffix(got.err, ": "+rule):
		t.Errorf("%q: the engine gives %v, and the rule %q", c.Src, got, rule)
		return
	case rule != "":
		for _, what := range []string{"no such table", "there is already a table named", "already exists", "no such column", "no such index"} {
			if strings.Contains(rule, what) {
				count(kind + " " + what)
			}
		}
		return
	}
	count(kind + " ok")
	ci, ok := s.(*stmt.CreateIndex)
	if !ok {
		return
	}
	columns := tables[ci.Table.Fold()]
	count(fmt.Sprint("create index of ", len(ci.Columns), " columns ok"))
	for _, ic := range ci.Columns {
		column := columns[stmttest.ColumnOf(columns, ic.Name)]
		count("create index of a column of type " + column.Type + " ok")
		if column.PrimaryKey {
			count("create index that holds the primary key ok")
		}
		if column.Nullable {
			count("create index of a column that takes NULL ok")
		}
		if ic.Descending {
			count("create index of a descending column ok")
		}
	}
}

// schema of the database of o, as the rules of [stmttest] take it: the columns of its tables, by the
// folds of their names, and the folds of the names of its indexes. It is read from SQLite before
// each statement, so it follows a roll back, and a drop table that took the indexes of its table
// along.
func (o *oracle) schema(t testing.TB) (map[string][]stmttest.TableColumn, map[string]bool) {
	t.Helper()
	tables := map[string][]stmttest.TableColumn{}
	for _, quoted := range o.run(t, "select name from sqlite_schema where type = 'table'").rows {
		name := unquoted(t, quoted)
		_, tables[fold(name)] = o.columns(t, name)
	}
	indexes := map[string]bool{}
	for _, quoted := range o.run(t, "select name from sqlite_schema where type = 'index' and sql is not null").rows {
		indexes[fold(unquoted(t, quoted))] = true
	}
	return tables, indexes
}

// existsKinds are the kinds of SQLite's errors of a statement of IF NOT EXISTS or IF EXISTS that
// [reachExists] counts, without the names in them.
var existsKinds = []string{
	"there is already an index named", "there is already a table named", "no such table", "no such column", "duplicate column name",
	"too many columns on", "string or blob too big", "attempt to write a readonly database",
}

// reachExists counts what the statement s of IF NOT EXISTS or IF EXISTS of slice 10 of the call c,
// whose answer of the engine is got, reaches on the database of o, before the call, and checks it
// against [stmttest.ExistsRule]: where the rule says that SQLite does nothing, the engine must
// succeed, and the statement counts by its kind, such as "drop table if exists", and the rule's
// reason; otherwise it counts as "did its work" if it succeeded, and else by the kind of its error, of
// [existsKinds], or "not supported". It reports whether the statement did nothing by the rule. It
// counts nothing and reports false for a statement without the clause, and for a call with
// arguments, which the rule of arguments decides.
func reachExists(t testing.TB, o *oracle, s stmt.Statement, c stmttest.Call, got answer, count func(string)) bool {
	t.Helper()
	var kind string
	switch s := s.(type) {
	case *stmt.CreateTable:
		kind = map[bool]string{true: "create table if not exists"}[s.IfNotExists]
	case *stmt.DropTable:
		kind = map[bool]string{true: "drop table if exists"}[s.IfExists]
	case *stmt.CreateIndex:
		kind = map[bool]string{true: "create index if not exists"}[s.IfNotExists]
	case *stmt.DropIndex:
		kind = map[bool]string{true: "drop index if exists"}[s.IfExists]
	}
	if kind == "" || len(c.Args) > 0 {
		return false
	}
	tables, indexes := o.schema(t)
	if rule := stmttest.ExistsRule(tables, indexes, s); rule != "" {
		if got.err != "" {
			t.Errorf("%q: the engine gives %v, and the rule does nothing: %v", c.Src, got, rule)
		}
		count(kind + " did nothing: " + rule)
		return true
	}
	outcome := got.err
	switch got.err {
	case "":
		outcome = "did its work"
	case "not supported":
	default:
		if i := slices.IndexFunc(existsKinds, func(k string) bool { return strings.Contains(got.err, k) }); i >= 0 {
			outcome = existsKinds[i]
		}
	}
	count(kind + " " + outcome)
	return false
}

// accessOf the access a of the engine, in the terms of [stmttest.ChosenAccess].
func accessOf(a engine.Access) stmttest.Access {
	value := func(v *s4qlpb.Value) any {
		switch {
		case v == nil:
			return nil
		case v.HasInteger():
			return v.GetInteger()
		case v.HasReal():
			return v.GetReal()
		case v.HasText():
			return v.GetText()
		case v.HasBlob():
			return v.GetBlob()
		}
		return nil
	}
	m := stmttest.Access{None: a.None, Get: a.Point != nil, Full: a.Full, Index: a.Index, LowerOpen: a.LowerOpen, UpperOpen: a.UpperOpen,
		Delivered: a.Delivered, Backward: a.Delivered && a.Backward, Sorted: a.Sorted}
	for _, v := range append(slices.Clone(a.Point), a.Equal...) {
		m.Pins = append(m.Pins, value(v))
	}
	m.Lower, m.Upper = value(a.Lower), value(a.Upper)
	return m
}

// checkChoice of the access of the select, the update, or the delete of the call c, whose WHERE is
// that of the select s with the arguments args, on the table of the name and the columns in o:
// unless s4ql or SQLite refuses it, the engine's choice, as [engine.AccessOf] gives it, must be the
// model's, as [stmttest.ChosenAccess] gives it. It counts the choice under "chosen", by its kind.
func checkChoice(t testing.TB, o *oracle, s *stmt.Select, args []any, c stmttest.Call, table string, columns []stmttest.TableColumn, count func(string)) {
	t.Helper()
	cis, indexes := o.tableIndexes(t, table, columns)
	want, ok := stmttest.ChosenAccess(columns, indexes, s, args)
	if !ok {
		return
	}
	a, ok := engine.AccessOf(c.Src, createTableOf(table, columns), cis, values(c.Args))
	if !ok || !accessOf(a).Same(want) {
		t.Errorf("%q with %#v: the engine chooses %v, and the model %v", c.Src, c.Args, accessOf(a), want)
		return
	}
	var kind string
	switch {
	case want.None:
		kind = "no row"
	case want.Get:
		kind = "a get"
	case want.Full:
		kind = "every row"
	default:
		kind = "an index"
		if want.Index == "" {
			kind = "the primary key"
		}
		kind += fmt.Sprint(" with ", len(want.Pins), " pinned")
		if want.Lower != nil || want.Upper != nil {
			kind += " and a range"
		}
	}
	if len(cis) > 1 {
		kind += " of a table of several indexes"
	}
	switch {
	case want.Delivered && want.Backward:
		kind += ", delivered backward"
	case want.Delivered:
		kind += ", delivered forward"
	case want.Sorted:
		kind += ", sorted"
	}
	count("chosen " + kind)
	// The choices that a condition with its value on the left decides: with != in place of the
	// operator of each, which pins and bounds nothing and keeps a comparison with NULL, the model
	// chooses another access.
	rest, valueFirst := *s, false
	rest.Where = slices.Clone(s.Where)
	for i, w := range rest.Where {
		if w.ValueFirst {
			rest.Where[i].Operator, valueFirst = stmt.OpNotEqual, true
		}
	}
	if without, ok := stmttest.ChosenAccess(columns, indexes, &rest, args); valueFirst && ok && !without.Same(want) {
		count("chosen " + kind + " by a value on the left")
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

// reachReturning counts what the insert, the update, or the delete s with RETURNING, of slice 14,
// reaches, whose answer of the engine through the call c is got, with the kind of its outcome, on the
// table as SQLite has it on o before the statement: the statement by the call, Query or Exec, and its
// outcome; the form of the clause, *, names, a name twice, and a name that the table does not have;
// for an update, a SET of the key and of a column of an index; for an update or a delete that fails
// on a constraint, whether its WHERE finds two rows or more, so that it fails after its first row;
// and for a query that succeeds, whether it gives no row, one, or several, and the types of the
// columns that it gives, and NULL among its values. It counts nothing for another statement.
func reachReturning(t testing.TB, o *oracle, s stmt.Statement, c stmttest.Call, got answer, outcome string, count func(string)) {
	t.Helper()
	var (
		kind  string
		table stmt.Name
		r     stmt.Returning
	)
	switch s := s.(type) {
	case *stmt.Insert:
		kind, table, r = "insert", s.Table, s.Returning
	case *stmt.Update:
		kind, table, r = "update", s.Table, s.Returning
	case *stmt.Delete:
		kind, table, r = "delete", s.Table, s.Returning
	}
	if !r.Present() {
		return
	}
	kind += " with RETURNING"
	count(kind + map[bool]string{true: " through Query ", false: " through Exec "}[c.Query] + outcome)
	name, columns := o.columns(t, table.Value)
	if columns == nil {
		return
	}
	var positions []int
	for _, n := range r.Columns {
		position := stmttest.ColumnOf(columns, n)
		if position < 0 {
			count(kind + " of a name that the table does not have " + outcome)
			return
		}
		positions = append(positions, position)
	}
	switch {
	case r.Columns == nil:
		count(kind + " * " + outcome)
		for i := range columns {
			positions = append(positions, i)
		}
	case len(slices.Compact(slices.Sorted(slices.Values(positions)))) < len(positions):
		count(kind + " of a name twice " + outcome)
	default:
		count(kind + " of names " + outcome)
	}
	args := oracleArgs(c.Args)
	if u, ok := s.(*stmt.Update); ok {
		_, indexes := o.tableIndexes(t, name, columns)
		for _, a := range u.Set {
			position := stmttest.ColumnOf(columns, a.Column)
			switch {
			case position < 0:
			case columns[position].PrimaryKey:
				count(kind + " that sets the key " + outcome)
			case slices.ContainsFunc(indexes, func(ix stmttest.TableIndex) bool {
				return slices.ContainsFunc(ix.Columns, func(k stmttest.IndexKeyColumn) bool { return k.Position == position })
			}):
				count(kind + " that sets a column of an index " + outcome)
			}
		}
	}
	if strings.Contains(outcome, "constraint failed") {
		if sel, selArgs := stmttest.SelectOf(s, args); sel != nil {
			src, whereArgs := stmttest.SelectText(sel, selArgs, "")
			if rows := o.run(t, "select count(*) from ("+src+")", whereArgs...); rows.err == "" && rows.rows[0] != "0" && rows.rows[0] != "1" {
				count(kind + " that fails on a constraint where its WHERE finds two rows or more")
			}
		}
	}
	if !c.Query || got.err != "" {
		return
	}
	switch len(got.rows) {
	case 0:
		count(kind + " that gives no row")
	case 1:
		count(kind + " that gives one row")
	default:
		count(kind + " that gives several rows")
	}
	if len(got.rows) == 0 {
		return
	}
	for _, typ := range slices.Compact(slices.Sorted(func(yield func(string) bool) {
		for _, position := range positions {
			if !yield(columns[position].Type) {
				return
			}
		}
	})) {
		count(kind + " that gives a column of " + typ)
	}
	if got.null {
		count(kind + " that gives NULL")
	}
	// * of a table with a column without a default after one with a default, where the order of the
	// table shows against an order that puts the columns with a default last.
	if r.Columns == nil {
		withDefault := false
		for _, c := range columns {
			if withDefault && c.Default == "" {
				count(kind + " * that gives a row of a table with a column without a default after one with a default")
				break
			}
			withDefault = withDefault || c.Default != ""
		}
	}
	// goqite's send and its receive by id, as far as s4ql takes them: an insert that leaves out a key
	// with a default and gives the key, and an update of the row of a key by an argument, of columns
	// outside the key, that gives the key and more.
	key := slices.IndexFunc(columns, func(c stmttest.TableColumn) bool { return c.PrimaryKey })
	switch s := s.(type) {
	case *stmt.Insert:
		if len(got.rows) == 1 && slices.Contains(positions, key) && s.Columns != nil && columns[key].Default != "" &&
			!slices.ContainsFunc(s.Columns, func(n stmt.Name) bool { return stmttest.ColumnOf(columns, n) == key }) {
			count("goqite's send: an insert that leaves out a key with a default and gives the key by RETURNING")
		}
	case *stmt.Update:
		if len(got.rows) == 1 && slices.Contains(positions, key) && len(positions) > 1 && len(s.Where) == 1 && s.Where[0].Operator == stmt.OpEqual &&
			s.Where[0].Value.Parameter > 0 && stmttest.ColumnOf(columns, s.Where[0].Column) == key &&
			!slices.ContainsFunc(s.Set, func(a stmt.Assignment) bool { return stmttest.ColumnOf(columns, a.Column) == key }) {
			count("goqite's receive by id: an update of the row of a key by an argument that gives the row by RETURNING")
		}
		// An update that gives the key of a row that it moved to another key: the key of its SET, which
		// no row had before the update, which SQLite has not run yet.
		if at := slices.IndexFunc(s.Set, func(a stmt.Assignment) bool { return stmttest.ColumnOf(columns, a.Column) == key }); at >= 0 && slices.Contains(positions, key) {
			src := "select count(*) from " + stmttest.Quote(name) + " where " + stmttest.Quote(columns[key].Name) + " = ?"
			if before := o.run(t, src, termArg(s.Set[at].Value, args)); before.err == "" && before.rows[0] == "0" {
				count(kind + " of the key that moves a row to another key")
			}
		}
	}
	if insert, ok := s.(*stmt.Insert); ok && insert.Columns != nil {
		for _, position := range positions {
			if !slices.ContainsFunc(insert.Columns, func(n stmt.Name) bool { return stmttest.ColumnOf(columns, n) == position }) {
				count(kind + " that gives a column that its list leaves out, " + map[bool]string{true: "with", false: "without"}[columns[position].Default != ""] + " a default")
			}
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
	table, columns := o.columns(t, s.Table.Value)
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
		for _, what := range stmttest.DefaultCases(columns, s, args) {
			count(what + " " + outcome)
		}
		// A default, of slice 11, that an index, of slice 9, holds: a meeting of two areas that only
		// the mixed profile has on.
		if _, indexes := o.tableIndexes(t, table, columns); stmttest.LeavesOutIndexed(columns, indexes, s) {
			count("insert that leaves out a column with a default that an index holds " + outcome)
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
// record that is too long in docs/todo/sql-front-end.md: it finds two rows or more, and when the rows change one at
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
// docs/todo/sql-front-end.md.
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
// [stmttest.DeleteRule] give it for an update and a delete, and after that for the argument of the
// LIMIT of a select, as [limitRule] gives it; or "". It is "" for another text, a
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
		if s.Where == nil && !s.Limited() || o.prepare(t, src) != nil {
			return ""
		}
		table, columns := o.columns(t, s.Table.Value)
		if rule := stmttest.ConditionRule(table, columns, s, args); rule != "" {
			return rule
		}
		return limitRule(s, args)
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

// limitRule of the select s with the arguments args as SQLite binds them, one for each parameter of
// s: s4ql's "not supported" for an argument of its LIMIT that is not an integer from 0, which SQLite
// would convert to an integer, take for no limit, or refuse with `datatype mismatch`; or "". A literal
// of LIMIT is always an integer from 0, since the statement pass refuses every other.
func limitRule(s *stmt.Select, args []any) string {
	if s.Limit.Parameter == 0 {
		return ""
	}
	which := limitKind(args[s.Limit.Parameter-1])
	if which == "" {
		return ""
	}
	return fmt.Sprintf("s4ql: not supported: argument %v, which is %v, for LIMIT, which takes an integer from 0", s.Limit.Parameter, which)
}

// limitKind of the argument a of LIMIT that s4ql refuses, as its message names it: "a negative
// integer", "a real", "text", "a blob", or "NULL"; and "" for an int64 from 0, which runs, and for an
// argument that no column holds, which [checkArguments] checks.
func limitKind(a any) string {
	switch a := a.(type) {
	case int64:
		if a < 0 {
			return "a negative integer"
		}
	case float64:
		return "a real"
	case string:
		return "text"
	case []byte:
		return "a blob"
	case nil:
		return "NULL"
	}
	return ""
}

// limitRefused reports whether s is a select whose argument of LIMIT, of args, s4ql refuses, as
// [limitRule] says.
func limitRefused(s stmt.Statement, args []any) bool {
	sel, ok := s.(*stmt.Select)
	return ok && len(args) == sel.Parameters() && limitRule(sel, args) != ""
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
	s, err := o.c.Prepare(`select m.name, p.name, p.type, p.pk, p."notnull", p.dflt_value from sqlite_schema m, pragma_table_info(m.name) p where m.type = 'table' and m.name = ?1 collate nocase order by p.cid`)
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
		// A column without DEFAULT has the dflt_value NULL, and the text of a literal is never empty.
		columns = append(columns, stmttest.TableColumn{Name: s.ColumnText(1), Type: s.ColumnText(2), PrimaryKey: key, Nullable: !key && s.ColumnInt64(4) == 0, Default: s.ColumnText(5)})
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

// variantOf the seed of a sequence of [TestDifferential]: whether its instance restarts, and the
// length limit of its cache and of SQLite, [stmttest.LengthLimit] or 0 for SQLite's default. Both
// come from the number of the seed among the seeds of its profile, so that each profile runs with
// every pair of them.
func variantOf(seed uint64) (restarts bool, limit int) {
	k := seed / stmttest.Slots
	return k%2 == 1, map[bool]int{true: stmttest.LengthLimit}[k%4 >= 2]
}

// sequencesOf the profile p in a test that runs n sequences of each profile: n, twice as many of
// the profiles of the updates and deletes, the indexes, the clauses of slice 10, and the values on
// the left, whose kinds of outcome are the most and the rarest, and five times as many of the mixed
// profile. The mixed profile is the only one in which every area meets the others, such as a default
// with an index or a clause of slice 10 with a value on the left, and no tally of a focused profile
// counts what only such a meeting reaches. With the default n, it runs 200 sequences, two thirds of
// the 300 of the one sequence of all areas before the profiles.
func sequencesOf(p *stmttest.Profile, n int) uint64 {
	switch p {
	case stmttest.Mixed:
		n *= 5
	case stmttest.Changes, stmttest.Indexes, stmttest.Exists, stmttest.ValueFirst, stmttest.Order, stmttest.Returning:
		n *= 2
	}
	return uint64(n)
}

// checkReached checks what the runs of the test reached, by the name of each, against the file of
// the test in testdata/reached, or writes the file with S4QL_RECOUNT=1, as [stmttest.Reached]
// says, for the kinds of the file of the test with the ending .kinds there. With least, it checks
// the least numbers alone, as [stmttest.Reached.Least] does, for runs of more sequences than those
// of the file. The kinds whose name begins with "excused: " are pinned exactly, since they tell how
// much of a test escapes the comparison with SQLite, but for [excusedRecordSlip], whose count
// depends on the allocator of the platform and which is left out.
func checkReached(t *testing.T, doc string, names []string, runs map[string]tally, least bool) {
	t.Helper()
	counts := map[string]map[string]int{}
	for name, y := range runs {
		counts[name] = maps.Clone(y)
		delete(counts[name], "excused: "+excusedRecordSlip)
	}
	path := filepath.Join("testdata", "reached", t.Name())
	reached := stmttest.Reached{Kinds: path + ".kinds", Counts: path + ".tsv", Doc: doc, Exact: func(kind string) bool { return strings.HasPrefix(kind, "excused: ") }}
	check := reached.Check
	if least {
		noRecount(t)
		check = reached.Least
	}
	failures, err := check(names, counts)
	is.NotError(t, err)
	for _, failure := range failures {
		t.Error(failure)
	}
}

// noRecount fails the test if S4QL_RECOUNT=1 asks for a count that the run cannot give, since it is
// not the full run with the default flags that the counts are of.
func noRecount(t *testing.T) {
	t.Helper()
	if stmttest.Recounting() {
		t.Errorf("%v=1 needs the full run of the test: without -short, the race detector, and the address sanitizer, with the default flags and seeds, and with every subtest", stmttest.RecountVariable)
	}
}

// flagSet reports whether the flag of the name was given to the test binary, whatever its value.
func flagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// TestDifferential runs random sequences of statements of each profile of [stmttest.Profiles] on
// the engine and on SQLite, and requires the same answers, the same count of changes, and the same
// tables and rows, half of them with restarts of the instance, with an empty cache and with its
// own, and half of them with the length limit lowered to [stmttest.LengthLimit].
//
// What the full run of each profile must reach is in testdata/reached/TestDifferential.tsv: every
// kind that [pair.reach] and the tally count three times or more in the profile, at least a third as
// often, and the texts that the run excuses from the comparison with SQLite, by kind, exactly. Of
// the calls that are excused because s4ql refuses their arguments by its rule, each is checked
// against the rule instead. After a change of the generator, S4QL_RECOUNT=1 writes the file again.
// The profiles together must reach each kind at least as often as the number before it in
// testdata/reached/TestDifferential.kinds, which is written by hand, so that a recount cannot lower
// a floor.
//
// With the flag -seed, also -seed 0, the test runs -sequences sequences from that seed, each with
// the profile of its seed, and checks no count. With more -sequences than the default, it runs that
// many of each profile, those of the full run among them, and checks the least numbers alone.
func TestDifferential(t *testing.T) {
	replayed := flagSet("seed")
	n := *sequences
	if slow() && !replayed {
		n /= 10
	}
	total := tally{}
	runs := map[string]tally{}
	var names []string
	run := func(s uint64) {
		p := stmttest.ProfileOf(s)
		if runs[p.Name] == nil {
			runs[p.Name] = tally{}
			names = append(names, p.Name)
		}
		restarts, limit := variantOf(s)
		differential(t, s, *length, restarts, limit, runs[p.Name])
	}
	if replayed {
		for s := range uint64(n) {
			run(*seed + s)
		}
	} else {
		for _, p := range stmttest.Profiles {
			for k := range sequencesOf(p, n) {
				run(p.Seed(k))
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("no sequence ran: -sequences is 0, or below 10 under -short, the race detector, or the address sanitizer")
	}
	for _, name := range names {
		t.Logf("%v, %v statements in each sequence: %v", name, *length, runs[name])
		for kind, count := range runs[name] {
			total[kind] += count
		}
	}
	t.Logf("%v calls ran in SQLite, and were compared with it", total[comparedWithSQLite])
	const doc = "What each profile of TestDifferential of the engine reaches in its full run."
	switch full := !slow() && !replayed && atDefault("length"); {
	case full && atDefault("sequences"):
		checkReached(t, doc, names, runs, false)
		// A floor under the calls that run in SQLite: the 12,108 of the run of 2026-11-20.
		is.True(t, total[comparedWithSQLite] >= 12108, "the calls that run in SQLite")
		if total["excused: "+excusedLong] == 0 {
			t.Errorf("no literal is longer than the length limit")
		}
	case full && n > flagDefault("sequences"):
		// More sequences of each profile, those of the full run among them.
		checkReached(t, doc, names, runs, true)
	default:
		noRecount(t)
	}
	// The outcomes of a parameter that every run must reach, also the tenth of the sequences of a slow
	// run: an insert through a parameter that succeeds, that meets a key that exists, and that the
	// rule for arguments refuses, and an argument for a statement without a parameter; and the errors
	// of the names of slice 5.
	if !replayed {
		for _, kind := range []string{
			"insert with a parameter ok", "insert with a parameter UNIQUE constraint failed", "insert with a parameter wrong number of arguments",
			"insert with a parameter not supported", "insert wrong number of arguments", "select wrong number of arguments",
			"createtable duplicate column name", "select no such column", "insert NOT NULL constraint failed", "insert values were supplied", "insert  values for ",
		} {
			if total[kind] == 0 {
				t.Errorf("no statement has the outcome %q", kind)
			}
		}
	}
}

// flagDefault is the default value of the flag of the name, which is a number.
func flagDefault(name string) int {
	n, err := strconv.Atoi(flag.Lookup(name).DefValue)
	if err != nil {
		panic(err)
	}
	return n
}

// TestDifferential_plantedBugs runs the first sequences of a profile of [TestDifferential] with each
// planted bug of the choice of an access in the engines of both instances of the pair, and requires
// that at least a pinned number of them find the bug, by a difference from SQLite, also on a path
// that [pair.everyPath] forces. The profile of a bug is the one that weighs what the bug breaks.
// Each sequence that finds the bug runs again without it and must find nothing, as every sequence of
// [TestDifferential] does.
func TestDifferential_plantedBugs(t *testing.T) {
	n := 30
	if slow() {
		n = 3
	}
	for _, b := range []struct {
		name    string
		profile *stmttest.Profile
		// least is the number of the n sequences that must find the bug by its own failure, a third of
		// what they found on 2026-11-21 and at least 1: 21 and 15 of 30, each by the rows of a select
		// or by the count of an update or a delete that differ from SQLite's, for the bugs of slice 13 2
		// of 30, 6 of 12, 4 of 9, 4 of 30, and 7 of 12, and for those of slice 14 21, 16, 13, and 16 of
		// 30 after the review of their checks on 2026-11-23, each by the rows of RETURNING that differ
		// from SQLite's.
		least int
		// finds holds a part of each failure that the bug causes, its own first, by which least of the
		// sequences must find it, or is nil for any failure. For the bugs of slice 13 its own is the
		// rows of an ordered select at a place where SQLite has other rows, as [ordered.compare] says.
		// A bug that changes the choice of an access is found by [pair.checkAccess] too, where the rows
		// that it gives are SQLite's, and a bug of the sort by the path that [pair.everyPath] forces;
		// for lastIndexWins, which changes nothing but the choice, its own is that of
		// [pair.checkAccess].
		finds []string
		// sequences is the number of the n sequences that the bug runs, for a bug that its sequences
		// find so often that fewer of them find it often enough, or 0 for all n.
		sequences int
	}{
		{"pinByPlace", stmttest.Indexes, 7, nil, 0},
		{"unturnedBound", stmttest.ValueFirst, 5, nil, 0},
		{"turnMixed", stmttest.Order, 1, []string{"where SQLite has the rows", "and the model chooses", "the select gives the rows"}, 0},
		{"limitFirst", stmttest.Order, 2, []string{"where SQLite has the rows", "the select gives the rows"}, 12},
		{"ascendLast", stmttest.Order, 1, []string{"where SQLite has the rows"}, 9},
		{"skipGap", stmttest.Order, 1, []string{"where SQLite has the rows", "and the model chooses", "the select gives the rows"}, 0},
		{"lastIndexWins", stmttest.Order, 2, []string{"and the model chooses"}, 12},
		{"oldValues", stmttest.Returning, 7, []string{"\nand SQLite\n"}, 0},
		{"deleteNone", stmttest.Returning, 5, []string{"\nand SQLite\n"}, 0},
		{"dropLast", stmttest.Returning, 4, []string{"\nand SQLite\n"}, 0},
		{"starKeyFirst", stmttest.Returning, 5, []string{"\nand SQLite\n"}, 0},
	} {
		t.Run(b.name, func(t *testing.T) {
			// own is the number of the sequences that find the bug by its own failure.
			found, own := 0, 0
			first := ""
			runs := n
			if b.sequences > 0 {
				runs = min(n, b.sequences)
			}
			for k := range uint64(runs) {
				s := b.profile.Seed(k)
				restarts, limit := variantOf(s)
				var options engine.Options
				options.Plant(b.name)
				i := newInstanceLimited(t, options, nil, limit)
				i.planted = true
				failure := differentialFind(t, s, *length, restarts, nil, i, options)
				i.close()
				if failure == "" {
					continue
				}
				if b.finds != nil && !slices.ContainsFunc(b.finds, func(f string) bool { return strings.Contains(failure, f) }) {
					t.Errorf("seed %v finds the bug by another failure than its own: %v", s, failure)
				}
				if b.finds == nil || strings.Contains(failure, b.finds[0]) {
					own++
				}
				found++
				if first == "" {
					first = failure[:min(len(failure), 2000)]
				}
				clean := newInstanceLimited(t, generatedOptions(), nil, limit)
				if failure := differentialFind(t, s, *length, restarts, nil, clean, generatedOptions()); failure != "" {
					t.Errorf("seed %v finds something without the bug: %v", s, failure)
				}
				clean.close()
			}
			t.Logf("%v of %v sequences of the profile %v find the bug, %v of them by its own failure; the first finds: %v", found, runs, b.profile.Name, own, first)
			if !slow() && atDefault("length") {
				is.True(t, own >= b.least, fmt.Sprint("the bug is found by its own failure too rarely: ", own))
			}
		})
	}
}

// TestPair_tooLongForCache pins the excuse of [pair.tooLongForCache] at the length limit of 1000: a
// create table that SQLite takes and whose statement in the cache is longer than the limit is
// excused, and [cacheStatementLength] measures that statement as the core does, byte for byte at
// the limit, for every kind of default.
func TestPair_tooLongForCache(t *testing.T) {
	// 42 columns of a real with the default .5, whose canonical form 5e-01 is three bytes longer: SQLite
	// takes the text under the limit of 1000, and the cache's statement is longer than the limit.
	table := func(n int, dflt string) string {
		var columns []string
		for i := range n {
			columns = append(columns, fmt.Sprintf("c%v real default%v", i, dflt))
		}
		return "create table t(k int primary key," + strings.Join(columns, ",") + ")strict,without rowid"
	}

	t.Run("should excuse a create table that SQLite takes and whose statement in the cache is longer than the limit, and measure that statement as the cache does", func(t *testing.T) {
		y := tally{}
		p := newPair(t, newInstanceLimited(t, engine.Options{}, nil, 1000), y)
		src := table(42, ".5")
		s, err := stmt.Check(src)
		is.NotError(t, err)
		is.True(t, cacheStatementLength(s.(*stmt.CreateTable)) > 1000)
		is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}))
		is.Equal(t, 1, y["excused: "+excusedCacheLimit])
		is.Equal(t, "", p.o.state(t), "SQLite has no table after the check")
		is.Equal(t, "", p.step(stmttest.Call{Src: table(10, ".5"), Supported: true}))
		is.Equal(t, 1, y["excused: "+excusedCacheLimit])
	})

	t.Run("should measure the statement in the cache as the core does: the core takes a table whose statement is as long as the limit, and refuses one a byte longer", func(t *testing.T) {
		for _, dflt := range []string{".5", "1e300", "-1.5e-300", "0.25"} {
			// Twenty-five reals with the default .5, whose canonical form makes the cache's statement longer
			// than SQLite's text, three with the default, a column of each other kind of default and a
			// name with a double quote, and a text default of n bytes, which moves the length byte by
			// byte.
			src := func(n int) string {
				var columns []string
				for i := range 25 {
					columns = append(columns, fmt.Sprintf("c%v real default.5", i))
				}
				for i := range 3 {
					columns = append(columns, fmt.Sprintf("d%v real default %v", i, dflt))
				}
				columns = append(columns, `"we""ird" int not null default 00012`, "b blob default X'aBcD'", "z real not null default -0.0", "u text default NULL",
					"q text default 'it''s'", "s text default '"+strings.Repeat("x", n)+"'")
				return "create table t(k int primary key," + strings.Join(columns, ",") + ")strict,without rowid"
			}
			length := func(n int) int {
				s, err := stmt.Check(src(n))
				is.NotError(t, err)
				return cacheStatementLength(s.(*stmt.CreateTable))
			}
			n := 1000 - length(0)
			is.True(t, n > 0, dflt)
			is.Equal(t, 1000, length(n), dflt)
			o := newOracleLimited(t, 1000)
			for _, m := range []int{n, n + 1} {
				is.Equal(t, "", o.run(t, src(m)).err, "SQLite takes the table")
				is.Equal(t, "", o.run(t, "drop table t").err)
				i := newInstanceLimited(t, engine.Options{}, nil, 1000)
				got := i.exec(src(m))
				if m == n {
					is.Equal(t, "", got.err, dflt)
					continue
				}
				is.Equal(t, "not supported", got.err, dflt)
				is.Equal(t, "s4ql: not supported: the table t, whose statement in the cache is longer than the length limit", got.message)
			}
		}
	})
}

// TestPair_recordSlips pins the known difference of a record that is too long, of docs/todo/sql-front-end.md, at the
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

func TestPair_sortTooBig(t *testing.T) {
	// The measure of the engine builder of slice 13, in docs/todo/slice-13.md: under a length limit of
	// 1,000 bytes, SQLite refuses a select that it sorts where the record of what it sorts of the
	// longest row that both take is longer than the limit, with a real that holds an integer at 8
	// bytes.
	setup := []string{
		"create table l (k int primary key, a text, b text, c real, d real) strict, without rowid",
		"insert into l values (1, '', '" + strings.Repeat("x", 990) + "', 4.0, 5.0)",
	}
	newSetUp := func(t *testing.T) (*pair, tally) {
		t.Helper()
		y := tally{}
		p := newPair(t, newInstanceLimited(t, engine.Options{}, nil, 1000), y)
		for _, src := range setup {
			is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
		}
		return p, y
	}

	t.Run("should excuse a sorted select that SQLite refuses for the length of what it sorts, and count it", func(t *testing.T) {
		p, y := newSetUp(t)
		for _, src := range []string{"select * from l order by b", "select b from l order by c", "select * from l order by a limit 1"} {
			is.Equal(t, tooBig, p.o.run(t, src).err, src)
			is.Equal(t, "", p.step(stmttest.Call{Src: src, Query: true, Supported: true}), src)
		}
		is.Equal(t, 3, y["excused: "+excusedSortTooBig])
	})

	t.Run("should excuse nothing where SQLite gives the rows", func(t *testing.T) {
		p, y := newSetUp(t)
		for _, src := range []string{"select k from l order by b", "select * from l order by k desc"} {
			is.Equal(t, "", p.step(stmttest.Call{Src: src, Query: true, Supported: true}), src)
		}
		is.Equal(t, 0, y["excused: "+excusedSortTooBig])
	})

	t.Run("should not excuse rows that are not SQLite's without the limit, nor a select without ORDER BY", func(t *testing.T) {
		p, _ := newSetUp(t)
		got := p.i.queryValues("select * from l order by b", nil)
		is.Equal(t, "", got.err)
		is.True(t, p.sortTooBig("select * from l order by b", nil, got))
		wrong := got
		wrong.rows = []string{strings.Replace(got.rows[0], "1", "2", 1)}
		is.True(t, !p.sortTooBig("select * from l order by b", nil, wrong))
		is.True(t, !p.sortTooBig("select * from l", nil, p.i.queryValues("select * from l", nil)))
	})

	t.Run("should not excuse a sorted select with a literal in a condition that is longer than the limit, which the rule of the length refuses", func(t *testing.T) {
		p, _ := newSetUp(t)
		src := "select k from l where a < '" + strings.Repeat("z", 1001) + "' order by k"
		is.Equal(t, tooBig, p.o.run(t, src).err)
		// The rows of an engine that ignores the rule of the length of a literal.
		ignores := p.o.unlimited(t, src)
		is.Equal(t, 1, len(ignores.rows))
		is.True(t, !p.sortTooBig(src, nil, ignores))
	})
}

func TestPair_orderBy(t *testing.T) {
	// run the calls on the pair, each of which must give what SQLite or s4ql's rule gives.
	run := func(t *testing.T, p *pair, calls []stmttest.Call) {
		t.Helper()
		for _, c := range calls {
			is.Equal(t, "", p.step(c), fmt.Sprintf("%v with %#v", c.Src, c.Args))
		}
	}
	// queries are the selects of pathsQueries, run as queries, and two as no query.
	var queries []stmttest.Call
	for _, q := range pathsQueries() {
		queries = append(queries, stmttest.Call{Src: q.src(), Args: q.args, Query: true, Supported: true})
	}
	queries = append(queries, stmttest.Call{Src: "select k from u order by s limit 2", Supported: true},
		stmttest.Call{Src: "select * from u where i = 1 order by r desc", Supported: true})
	long := strings.Repeat("x", 1001)
	// refused are selects with ORDER BY or LIMIT that SQLite or s4ql's rules refuse, in the order of
	// the errors of the engine.
	refused := []stmttest.Call{
		{Src: "select k from u order by x", Query: true},
		{Src: `select k from u where i = ? order by "x" limit ?`, Args: []any{"text", 2.5}, Query: true},
		{Src: "select k from u limit ?", Query: true},
		{Src: "select k from u order by s limit ?", Args: []any{math.NaN()}, Query: true},
		{Src: "select k from u where s = ? order by k limit ?", Args: []any{long, 2.5}, Query: true},
		{Src: "select k from u where i = ? order by k limit ?", Args: []any{"text", []byte(long)}, Query: true},
		{Src: "select k from u where i = ? order by k limit ?", Args: []any{"text", 2.5}, Query: true},
		{Src: "select k from u where r = ? limit ?", Args: []any{int64(1), int64(2)}, Query: true},
	}
	for _, a := range []any{2.5, 2.0, "2", []byte("2"), []byte{}, nil, int64(-1), int64(math.MinInt64)} {
		refused = append(refused, stmttest.Call{Src: "select k from u order by s limit ?", Args: []any{a}, Query: true},
			stmttest.Call{Src: "select k from u where i = ? limit ?", Args: []any{int64(1), a}, Query: true},
			stmttest.Call{Src: "select * from u where n = ? and s > ? order by r desc, k limit ?", Args: []any{int64(2), "a", a}})
	}
	// begin a transaction, read-only if readOnly, on both instances of the pair p and on SQLite.
	begin := func(t *testing.T, p *pair, readOnly bool) {
		t.Helper()
		for _, c := range []*engine.Conn{p.i.c, p.other.c} {
			is.NotError(t, map[bool]func(context.Context) error{false: c.Begin, true: c.BeginReadOnly}[readOnly](t.Context()))
		}
		for _, src := range oracleBegin(readOnly) {
			is.Equal(t, "", p.o.run(t, src).err, src)
		}
		p.inTx, p.readOnly = true, readOnly
	}
	// rollback the transaction of begin.
	rollback := func(t *testing.T, p *pair) {
		t.Helper()
		for _, c := range []*engine.Conn{p.i.c, p.other.c} {
			is.NotError(t, c.Rollback(t.Context()))
		}
		for _, src := range oracleEnd(p.readOnly, false) {
			is.Equal(t, "", p.o.run(t, src).err, src)
		}
		p.inTx, p.readOnly = false, false
	}

	t.Run("should take selects with ORDER BY and LIMIT, by every path, and refusals of their arguments by s4ql's rules, outside a transaction and in each kind", func(t *testing.T) {
		y := tally{}
		p := newPair(t, newInstanceLimited(t, engine.Options{}, nil, 1000), y)
		p.allPaths = true
		var setUp, writes []stmttest.Call
		for _, src := range pathsSetUp {
			setUp = append(setUp, stmttest.Call{Src: src, Supported: true})
		}
		for _, src := range pathsWrites {
			writes = append(writes, stmttest.Call{Src: src, Supported: true})
		}
		run(t, p, setUp)
		run(t, p, queries)
		run(t, p, refused)
		begin(t, p, false)
		run(t, p, writes)
		run(t, p, queries)
		run(t, p, refused)
		rollback(t, p)
		begin(t, p, true)
		run(t, p, queries)
		run(t, p, refused)
		rollback(t, p)
		is.Equal(t, p.o.state(t), p.i.state())
		is.Equal(t, p.o.state(t), p.other.state())
		var counts []string
		for _, kind := range slices.Sorted(maps.Keys(y)) {
			// The kinds of the order of the rows that tie and of those without ORDER BY depend on the
			// directions that the engines draw, from a seed of each run.
			if strings.Contains(kind, " came in the ") && !strings.HasPrefix(kind, "reached: delivered ") {
				continue
			}
			if strings.Contains(kind, "ORDER BY") || strings.Contains(kind, "LIMIT") || strings.HasPrefix(kind, "excused: ") || strings.Contains(kind, "sort") || strings.Contains(kind, "deliver") || strings.Contains(kind, "plan") {
				counts = append(counts, fmt.Sprint(kind, ": ", y[kind]))
			}
		}
		is.Equal(t, strings.Join([]string{
			"excused: a value of another type than its column, checked against s4ql's rule: 78",
			"excused: an argument that no column holds, checked against s4ql's rule: 3",
			"excused: the wrong number of arguments, checked against s4ql's rule: 3",
			"reached: LIMIT 0: 54",
			"reached: LIMIT above the number of rows: 54",
			"reached: LIMIT by an argument a value of another type: 3",
			"reached: LIMIT by an argument ok: 75",
			"reached: LIMIT of the number of rows: 6",
			"reached: LIMIT run by every path: 192",
			"reached: LIMIT that cuts: 69",
			"reached: LIMIT that cuts a group of rows that tie: 16",
			"reached: LIMIT with an argument that is NULL not supported: 9",
			"reached: LIMIT with an argument that is a blob not supported: 18",
			"reached: LIMIT with an argument that is a blob string or blob too big: 3",
			"reached: LIMIT with an argument that is a negative integer not supported: 18",
			"reached: LIMIT with an argument that is a real a value of another type: 3",
			"reached: LIMIT with an argument that is a real no such column: 3",
			"reached: LIMIT with an argument that is a real not supported: 18",
			"reached: LIMIT with an argument that is a real string or blob too big: 3",
			"reached: LIMIT with an argument that is text not supported: 9",
			"reached: ORDER BY ascending ok: 147",
			"reached: ORDER BY descending ok: 87",
			"reached: ORDER BY of 1 columns ok: 222",
			"reached: ORDER BY of 2 columns ok: 72",
			"reached: ORDER BY of 3 columns ok: 21",
			"reached: ORDER BY of a column of type BLOB ok: 48",
			"reached: ORDER BY of a column of type INT ok: 111",
			"reached: ORDER BY of a column of type INTEGER ok: 45",
			"reached: ORDER BY of a column of type REAL ok: 96",
			"reached: ORDER BY of a column of type TEXT ok: 129",
			"reached: ORDER BY of a column that takes NULL ok: 342",
			"reached: ORDER BY of a column that the table does not have no such column: 3",
			"reached: ORDER BY of mixed directions ok: 81",
			"reached: ORDER BY or LIMIT with arguments that s4ql refuses not supported: 3",
			"reached: ORDER BY or LIMIT with arguments that s4ql refuses wrong number of arguments: 3",
			"reached: ORDER BY over rows with NULL in its columns: 165",
			"reached: ORDER BY run by every path: 315",
			"reached: ORDER BY with LIMIT read by an index that delivers the order beside one that narrows a range: 6",
			"reached: ORDER BY without WHERE ok: 150",
			"reached: chosen a get of a table of several indexes, delivered forward: 21",
			"reached: chosen an index with 0 pinned and a range of a table of several indexes, delivered backward: 12",
			"reached: chosen an index with 0 pinned and a range of a table of several indexes, delivered forward: 6",
			"reached: chosen an index with 0 pinned and a range of a table of several indexes, sorted: 9",
			"reached: chosen an index with 0 pinned of a table of several indexes, delivered backward: 51",
			"reached: chosen an index with 0 pinned of a table of several indexes, delivered forward: 66",
			"reached: chosen an index with 1 pinned and a range of a table of several indexes, delivered backward: 6",
			"reached: chosen an index with 1 pinned of a table of several indexes, delivered backward: 27",
			"reached: chosen an index with 1 pinned of a table of several indexes, delivered forward: 33",
			"reached: chosen an index with 1 pinned of a table of several indexes, sorted: 6",
			"reached: chosen every row of a table of several indexes, delivered backward: 6",
			"reached: chosen every row of a table of several indexes, delivered forward: 9",
			"reached: chosen every row of a table of several indexes, sorted: 45",
			"reached: chosen no row of a table of several indexes, delivered forward: 9",
			"reached: chosen the primary key with 0 pinned and a range of a table of several indexes, delivered backward: 9",
			"reached: delivered select whose rows that tie came in the order of the key of its access: 96",
			"reached: path that delivers the order: 411",
			"reached: path that sorts: 4074",
			"reached: plan of the cache checked: 401",
		}, "\n"), strings.Join(counts, "\n"))
	})

	t.Run("should take them on a database in memory", func(t *testing.T) {
		p := newPair(t, newMemoryInstance(t), nil)
		p.allPaths = true
		for _, src := range pathsSetUp {
			is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
		}
		run(t, p, queries)
		run(t, p, refused)
	})
}

// FuzzDifferential runs the sequence of statements that its input chooses, the seed and the number of
// statements, as [TestDifferential] does, on an engine on a database in memory, a test double of the
// core. Its input is a seed of the generator, not SQL text, so the guidance of the fuzzer by coverage
// has little to work with: it explores seeds of the generator, and its executions are not a measure
// of the SQL text that it covers. [FuzzSQL] gives the engine any text.
func FuzzDifferential(f *testing.F) {
	f.Add(uint64(0), uint8(40))
	f.Add(uint64(1), uint8(10))
	// A seed of the profile of ORDER BY and LIMIT, of slice 13.
	f.Add(stmttest.Order.Seed(0), uint8(48))
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
	f.Add(stmttest.Order.Seed(1), uint8(48), false, true)
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

// shardedAdd gives a function that adds a seed of f, as f.Add does, if the shard of the test holds
// its place among the seeds, as [testshard.Get] gives the shard: every seed outside a run in shards,
// so that the shards of the sanitizer's job share the seeds of a fuzz test, which run as its
// subtests.
func shardedAdd(f *testing.F) func(args ...any) {
	shard := testshard.Get(f)
	i := 0
	return func(args ...any) {
		if shard.Holds(i) {
			f.Add(args...)
		}
		i++
	}
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

// fuzzSQL is [FuzzSQL] on the instances of newInstance. Its seeds are statements of a sequence of
// the mixed profile, the texts of [stmttest.Others] and [fuzzSetUp], and every 500th text of the corpus.
func fuzzSQL(f *testing.F, newInstance func(t testing.TB) *instance) {
	add := shardedAdd(f)
	q := newSequence(f, stmttest.Mixed.Seed(1))
	for range 40 {
		c := q.Next()
		add(c.Src, !c.Query)
	}
	for _, src := range slices.Concat(fuzzSetUp, stmttest.Others) {
		if !strings.Contains(src, "%") {
			add(src, false)
			continue
		}
		add(fmt.Sprintf(src, `"foo BAR"`, `"BAR""x"`, 7), false)
		add(fmt.Sprintf(src, "T1", "v1", 1), true)
	}
	for i, e := range corpus.All() {
		if i%500 == 0 {
			add(e.SQL, i%1000 == 0)
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
// NULL is refused by SQLite's NOT NULL in the key and in a column that does not take it. Then it
// creates a table of each type whose column has the value, as [typesLiteral] writes it, as its
// DEFAULT, of slice 11, inserts a row that leaves the column out and one that gives it NULL, and
// reads the table, each compared with SQLite as before. Then
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

// typesLiteral is the value of the bytes data in the form, as [typesCall] gives it, written as a
// literal, for a DEFAULT: the literal of a form of a literal, the argument of a form of an argument
// as a literal, an integer in decimal and a real in the shortest exponent form, which is NaN or
// +Inf for those, NULL for [formNull], and the bytes themselves for [formSQL].
func typesLiteral(data []byte, form uint8) string {
	var b [8]byte
	copy(b[:], data)
	bits := binary.LittleEndian.Uint64(b[:])
	switch form % forms {
	case formTextArgument, formStringLiteral:
		return "'" + strings.ReplaceAll(string(data), "'", "''") + "'"
	case formBlobArgument, formBlobLiteral:
		x := "x'" + hex.EncodeToString(data) + "'"
		if form/forms%2 == 1 {
			x = strings.ToUpper(x)
		}
		return x
	case formRealArgument:
		return strconv.FormatFloat(math.Float64frombits(bits), 'e', -1, 64)
	case formIntegerArgument:
		return fmt.Sprint(int64(bits))
	case formNull:
		return "null"
	}
	return string(data)
}

// fuzzTypes is [FuzzTypes] on the instances of newInstance, which takes the length limit, 0 for
// SQLite's default. Its seeds are values at the edges of each type in every form.
func fuzzTypes(f *testing.F, newInstance func(t testing.TB, limit int) *instance) {
	add := shardedAdd(f)
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
			add(data, form, uint8(0))
		}
		add(data, uint8(formBlobLiteral+forms), uint8(0))
	}
	// Values about the length limit of 1001 bytes, where a value or its record is too long.
	for n := 995; n <= 1002; n++ {
		for _, form := range []uint8{formTextArgument, formBlobArgument, formStringLiteral, formBlobLiteral + forms} {
			add([]byte(strings.Repeat("x", n)), form, uint8(1))
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
		// A table of each type whose column has the value of the form as its DEFAULT, of slice 11: an
		// insert that leaves the column out stores it, and one that gives NULL stores NULL. The pass
		// refuses a default that is not a literal of the type of the column, and SQLite's error, such
		// as a syntax error, comes as it does.
		for _, typ := range []string{"int", "integer", "real", "text", "blob"} {
			table := "t_" + typ + "_d"
			for _, c := range []stmttest.Call{
				{Src: "create table " + table + " (k int primary key, v " + typ + " default " + typesLiteral(data, form) + ") strict, without rowid"},
				{Src: "insert into " + table + " (k) values (1)"},
				{Src: "insert into " + table + " (k, v) values (2, null)"},
				{Src: "select * from " + table, Query: true},
			} {
				if failure := p.step(c); failure != "" {
					t.Fatal(failure)
				}
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
// argument, a column that the table does not have, and several conditions, also with the value on
// the left of slice 12.
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
		// Each operator of a comparison with its value on the left, of slice 12, and with conditions
		// of the other order.
		for op := range byte(8) {
			f.Add([]byte{table, op % 2, 130 + op, op, 2, 1, 0})
		}
		f.Add([]byte{table, 0, 128 + 4, 0, 1, 0, 2, 0, 3, 3, 0, 128 + 6, 13, 1, 0, 128 + 7, 9})
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
// quotes for a byte above 127; its operator, one of the eight of slice 7, which a byte above 127 writes
// with the value on the left of the column, of slice 12, and the operator turned round; and for a
// comparison its value, by a byte whose lowest bits choose a key of a row of the table, a value of
// typ from the bytes that follow, NULL, or a value of another type from the bytes that follow, and
// whose next bit chooses a literal or an argument.
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
	types := make([]string, len(names))
	for j := range types {
		types[j] = typ
	}
	return r.typedConditions(names, types, most, args)
}

// typedConditions are [fuzzBytes.conditions] on columns of the names, each of the type at its
// position in types. The byte that chooses the operator of a comparison writes it with its value on
// the left of its column, of slice 12, and the operator turned round, if its highest bit is set.
func (r *fuzzBytes) typedConditions(names, types []string, most int, args []any) (string, []any) {
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
		typ := types[int(col&0x7f)%len(names)]
		name := names[int(col&0x7f)%len(names)]
		if col > 127 {
			name = `"` + name + `"`
		}
		form := r.next()
		op := ops[int(form)%len(ops)]
		if strings.HasPrefix(op, "is") {
			b.WriteString(name + " " + op)
			continue
		}
		var term string
		term, args = r.term(typ, args)
		if form > 127 {
			b.WriteString(term + " " + turned[op] + " " + name)
			continue
		}
		b.WriteString(name + " " + op + " " + term)
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
		p.inTx = inTx
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

// FuzzIndexes runs, on the engine on a database in memory, a test double of the core, and on SQLite,
// a table of one to three columns with one to three indexes, and inserts, updates, deletes, selects,
// drops of an index, and creates of one on it, also with IF EXISTS and IF NOT EXISTS of slice 10, and
// of the table, all of which the bytes of its input choose as
// [indexesCalls] says, and compares what each gives, as [TestDifferential] does. Each WHERE runs by
// every path that can serve it, and the engine's choice of a path must be that of
// [stmttest.ChosenAccess]: see [pair.everyPath] and [checkChoice]. Then it compares the tables and
// their indexes. [FuzzIndexes_core] runs the same on the storage core.
func FuzzIndexes(f *testing.F) {
	fuzzIndexes(f, newMemoryInstance)
}

// FuzzIndexes_core is [FuzzIndexes] on the storage core, with a store in memory. After the calls, it
// replays the log into an empty cache and compares the tables and their indexes again.
func FuzzIndexes_core(f *testing.F) {
	fuzzIndexes(f, func(t testing.TB) *instance { return newInstance(t, engine.Options{}) })
}

// fuzzIndexes is [FuzzIndexes] on the instances of newInstance. Its seeds are those of
// [indexesSeeds].
func fuzzIndexes(f *testing.F, newInstance func(t testing.TB) *instance) {
	for _, data := range indexesSeeds() {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		indexesRun(t, newInstance(t), data, tally{})
	})
}

// indexesRun runs the calls of [indexesCalls] of data on the instance i and SQLite, through a pair
// whose tally is y and which runs each WHERE by every path, and compares the tables and their
// indexes at the end, also after a replay of the log into an empty cache on the core, after which it
// runs the selects again.
func indexesRun(t *testing.T, i *instance, data []byte, y tally) {
	t.Helper()
	p := newPair(t, i, y)
	p.allPaths = true
	var log []string
	calls := indexesCalls(data)
	for _, c := range calls {
		log = append(log, fmt.Sprintf("%v -- with %#v", c.Src, c.Args))
		if failure := p.step(c); failure != "" {
			t.Fatalf("after\n%v\n%v", strings.Join(log[:len(log)-1], "\n"), failure)
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
	// The selects again, which read by the indexes of the replay.
	for _, c := range calls {
		if !c.Query {
			continue
		}
		if failure := p.step(c); failure != "" {
			t.Fatalf("after\n%v\nand a replay into an empty cache\n%v", strings.Join(log, "\n"), failure)
		}
	}
}

// indexesIndex is an index of the table of [indexesCalls]: its name, and its columns, each as its
// position in the table and whether it is descending.
type indexesIndex struct {
	name       string
	columns    []int
	descending []bool
}

// indexesCalls of the bytes data: a table, its indexes, its rows, and up to 16 statements on it.
//
// The first byte chooses the table: its lowest seven bits, modulo 3, one to three columns, and,
// divided by 3, the position of the key, and its highest bit the names. The names are c0 to c2, or
// queue, priority, and created, and the table is t, or goqite. A byte for each column chooses by its
// lowest three bits, modulo 5, its type, one of the five of [whereTypes], and by its bit 8 whether a
// column outside the key takes NULL.
//
// The next byte, modulo 3, chooses one to three indexes, which [fuzzBytes.index] reads, named i0 to
// i2. An index is created before the rows, or after them if the highest bit of its first byte is
// set. The rows are as many as [whereRows] has of the type of the key: the j-th has the j-th of them
// as its key, and in each other column, at the position c, the value of [whereRows] of its type at
// j/2 + c, or NULL where the column takes it and j + c is 1 modulo 3, so that pairs of rows share
// values.
//
// Each statement after them takes a byte, whose lowest three bits choose its kind and whose others,
// a, what it is of: for 0, a select of * with a WHERE of a%3+1 conditions, as
// [fuzzBytes.typedConditions] gives them on the columns of the table; for 1 and 7, a select of *
// that pins and bounds the key of the index a modulo the indexes that the table has, or of the
// primary key if it has none, as [fuzzBytes.pins] says; for 2, an insert of a value of each column,
// as [fuzzBytes.term] gives it; for 3, an update of the column a modulo the columns to a term, with a
// WHERE of a/4%2+1 conditions; for 4, a delete with a WHERE of a%2+1 conditions; for 5, a drop of the
// index a modulo the indexes, or of i0 if there are none; and for 6, a create of an index: again of
// the last that was dropped, for an odd a where one was, and otherwise of a new one that
// [fuzzBytes.index] reads, named i3 and on.
//
// The bits 4, 8, and 16 of a turn a drop and a create into the statements of slice 10. With 4, the
// statement says IF EXISTS or IF NOT EXISTS. With 16, a drop is of the table, and a create is of
// the table again, of its columns, which takes the rows and the indexes along, or does nothing. With
// 8, a drop of an index names one that is not there, i9, or the table for an odd a, and a drop of the
// table names an index that is there, or i9; and a create of an index names an index that is there,
// the one of a modulo the indexes, with new columns.
func indexesCalls(data []byte) []stmttest.Call {
	r := &fuzzBytes{data: data}
	shape := r.next()
	n := int(shape&0x7f)%3 + 1
	key := int(shape&0x7f) / 3 % n
	table, names := "t", []string{"c0", "c1", "c2"}
	if shape&0x80 != 0 {
		table, names = "goqite", []string{"queue", "priority", "created"}
	}
	names = names[:n]
	types := make([]string, n)
	nullable := make([]bool, n)
	var columns []string
	for j := range n {
		b := r.next()
		types[j] = whereTypes[int(b&7)%len(whereTypes)]
		nullable[j] = b&8 != 0 && j != key
		column := names[j] + " " + types[j]
		switch {
		case j == key:
			column += " primary key"
		case !nullable[j]:
			column += " not null"
		}
		columns = append(columns, column)
	}
	calls := []stmttest.Call{{Src: "create table " + table + " (" + strings.Join(columns, ", ") + ") strict, without rowid", Supported: true}}
	create := func(ix indexesIndex) stmttest.Call {
		var cs []string
		for j, position := range ix.columns {
			c := names[position]
			if ix.descending[j] {
				c += " desc"
			}
			cs = append(cs, c)
		}
		return stmttest.Call{Src: "create index " + ix.name + " on " + table + " (" + strings.Join(cs, ", ") + ")", Supported: true}
	}
	var live, dropped, after []indexesIndex
	// gone is set while the table is dropped.
	gone := false
	for j := range int(r.next())%3 + 1 {
		ix, later := r.index(fmt.Sprint("i", j), n)
		if later {
			after = append(after, ix)
		} else {
			calls = append(calls, create(ix))
		}
		live = append(live, ix)
	}
	insert := "insert into " + table + " values (" + strings.Repeat("?, ", n-1) + "?)"
	for j, k := range whereRows[types[key]] {
		row := make([]any, n)
		for c := range n {
			values := whereRows[types[c]]
			switch {
			case c == key:
				row[c] = k
			case nullable[c] && (j+c)%3 == 1:
			default:
				row[c] = values[(j/2+c)%len(values)]
			}
		}
		calls = append(calls, stmttest.Call{Src: insert, Args: row, Supported: true})
	}
	for _, ix := range after {
		calls = append(calls, create(ix))
	}
	next := 3
	for s := 0; s < 16 && len(r.data) > 0; s++ {
		b := r.next()
		a := int(b >> 3)
		c := stmttest.Call{Supported: true}
		switch b % 8 {
		case 0:
			var where string
			where, c.Args = r.typedConditions(names, types, a%3+1, nil)
			c.Src, c.Query = "select * from "+table+" where "+where, true
		case 1, 7:
			ix := indexesIndex{columns: []int{key}, descending: []bool{false}}
			if len(live) > 0 {
				ix = live[a%len(live)]
			}
			var where string
			where, c.Args = r.pins(ix, key, names, types)
			c.Src, c.Query = "select * from "+table+" where "+where, true
		case 2:
			terms := make([]string, n)
			for j := range n {
				terms[j], c.Args = r.term(types[j], c.Args)
			}
			c.Src = "insert into " + table + " values (" + strings.Join(terms, ", ") + ")"
		case 3:
			var term, where string
			term, c.Args = r.term(types[a%n], nil)
			where, c.Args = r.typedConditions(names, types, a/4%2+1, c.Args)
			c.Src = "update " + table + " set " + names[a%n] + " = " + term + " where " + where
		case 4:
			var where string
			where, c.Args = r.typedConditions(names, types, a%2+1, nil)
			c.Src = "delete from " + table + " where " + where
		case 5:
			clause := map[bool]string{true: "if exists "}[a&4 != 0]
			switch {
			case a&16 != 0 && a&8 != 0:
				name := "i9"
				if len(live) > 0 {
					name = live[a%len(live)].name
				}
				c.Src = "drop table " + clause + name
			case a&16 != 0:
				c.Src = "drop table " + clause + table
				if !gone {
					gone, dropped, live = true, append(dropped, live...), nil
				}
			case a&8 != 0:
				c.Src = "drop index " + clause + map[bool]string{false: "i9", true: table}[a%2 == 1]
			case len(live) == 0:
				c.Src = "drop index " + clause + "i0"
			default:
				j := a % len(live)
				c.Src = "drop index " + clause + live[j].name
				dropped = append(dropped, live[j])
				live = slices.Delete(live, j, j+1)
			}
		case 6:
			clause := map[bool]string{true: "if not exists "}[a&4 != 0]
			if a&16 != 0 {
				c.Src = "create table " + clause + table + " (" + strings.Join(columns, ", ") + ") strict, without rowid"
				gone = false
				break
			}
			var ix indexesIndex
			there := a&8 != 0 && len(live) > 0
			switch {
			case there:
				ix, _ = r.index(live[a%len(live)].name, n)
			case a%2 == 1 && len(dropped) > 0:
				ix, dropped = dropped[len(dropped)-1], dropped[:len(dropped)-1]
			default:
				ix, _ = r.index(fmt.Sprint("i", next), n)
				next++
			}
			c = create(ix)
			c.Src = strings.Replace(c.Src, "create index ", "create index "+clause, 1)
			if !there && !gone {
				live = append(live, ix)
			}
		}
		calls = append(calls, c)
	}
	return calls
}

// index of the name on a table of n columns: by a byte whose lowest seven bits, modulo n, choose one
// to n columns, and whose highest bit says whether the index is created after the rows, and a byte
// for each column, whose lowest seven bits choose it among the columns that the index does not hold
// yet, in the order of the table, and whose highest bit whether it is descending.
func (r *fuzzBytes) index(name string, n int) (indexesIndex, bool) {
	h := r.next()
	left := make([]int, n)
	for j := range left {
		left[j] = j
	}
	ix := indexesIndex{name: name}
	for range int(h&0x7f)%n + 1 {
		b := r.next()
		j := int(b&0x7f) % len(left)
		ix.columns = append(ix.columns, left[j])
		ix.descending = append(ix.descending, b&0x80 != 0)
		left = slices.Delete(left, j, j+1)
	}
	return ix, h&0x80 != 0
}

// pins of a WHERE, without the keyword, and their arguments, on the key of the index ix, its columns
// and then the key column key if it does not hold it, of a table of the columns of the names and the
// types: a byte, modulo the columns of the key plus one, chooses how many of them are pinned, and a
// byte for each pinned column whether it is IS NULL, for an odd byte, or = a term, as
// [fuzzBytes.term] gives it, with the term on the left of the column, of slice 12, if its bit 2 is
// set. A byte then bounds the next column of the key, if there is one: by its lowest two bits not,
// from below, from above, or both, from below with >= if its bit 4 is set and > otherwise, and from
// above with <= if its bit 8 is set and < otherwise, each with a term, which is on the left of the
// column, with the operator turned round, for the lower bound if its bit 16 is set and for the upper
// one if its bit 32 is. A WHERE that pins nothing and bounds nothing is the condition of the first
// column IS NOT NULL.
func (r *fuzzBytes) pins(ix indexesIndex, key int, names, types []string) (string, []any) {
	columns := slices.Clone(ix.columns)
	if !slices.Contains(columns, key) {
		columns = append(columns, key)
	}
	var conditions []string
	var args []any
	// condition of the column of the name by the operator op and the term, with the value on the left
	// of the column, of slice 12, and op turned round if valueFirst.
	condition := func(name, op, term string, valueFirst bool) string {
		if valueFirst {
			return term + " " + turned[op] + " " + name
		}
		return name + " " + op + " " + term
	}
	pinned := int(r.next()) % (len(columns) + 1)
	for _, c := range columns[:pinned] {
		form := r.next()
		if form%2 == 1 {
			conditions = append(conditions, names[c]+" is null")
			continue
		}
		var term string
		term, args = r.term(types[c], args)
		conditions = append(conditions, condition(names[c], "=", term, form&2 != 0))
	}
	bounds := r.next()
	if pinned < len(columns) {
		c := columns[pinned]
		if bounds&1 != 0 {
			var term string
			term, args = r.term(types[c], args)
			conditions = append(conditions, condition(names[c], map[bool]string{false: ">", true: ">="}[bounds&4 != 0], term, bounds&16 != 0))
		}
		if bounds&2 != 0 {
			var term string
			term, args = r.term(types[c], args)
			conditions = append(conditions, condition(names[c], map[bool]string{false: "<", true: "<="}[bounds&8 != 0], term, bounds&32 != 0))
		}
	}
	if conditions == nil {
		return names[columns[0]] + " is not null", args
	}
	return strings.Join(conditions, " and "), args
}

// indexesSeeds of [FuzzIndexes], built by hand: pins of one, two, and three columns, a pin of NULL,
// ranges on a descending column, indexes that hold the primary key, a drop of an index while the
// table has rows and selects after it, a create of the index again, goqite's table with its index on
// (queue, priority desc, created), and, for each type, a table of one column and a table of three.
// A term is [0, j] for the j-th value of [whereRows] of its type as a literal, [4, j] for it as an
// argument, and [2] for NULL.
func indexesSeeds() [][]byte {
	seeds := [][]byte{
		// t (c0 int primary key, c1 text, c2 real) with i0 (c1) before the rows and i1 (c2 desc, c0)
		// after them: c1 = 'a'; c1 is null; c1 = 'a' and c0 = 0; c2 > -1.5 and c2 < 1.5; and c2 = the
		// least real and c0 >= ? of the least integer.
		{2, 0, 11, 10, 1, 0, 1, 0x81, 0x82, 0,
			1, 1, 0, 0, 2, 0,
			1, 1, 1, 0,
			1, 2, 0, 0, 2, 0, 0, 2, 0,
			9, 0, 3, 0, 1, 0, 5,
			9, 1, 0, 0, 3, 5, 4, 0},
		// The same table: drop i0 while it has rows; a select of c1 = 'a'; a select by i1; create i0
		// again, and a select by it; and create i3 (c2 desc, c0 desc, c1), and pin its three columns.
		{2, 0, 11, 10, 1, 0, 1, 0x81, 0x82, 0,
			5,
			0, 1, 0, 0, 2,
			1, 1, 0, 0, 3, 0,
			14,
			9, 1, 0, 0, 2, 0,
			6, 2, 0x82, 0x80, 0,
			17, 3, 0, 0, 3, 0, 0, 2, 0, 0, 2, 0},
		// goqite (queue text, priority int, created text primary key) with i0 (queue, priority desc,
		// created) after the rows: pins of three; of two and a range on created; of queue and a range
		// on priority; an insert; an update of priority; pins of two after it; a delete; a select of
		// two conditions; a pin of queue to an integer; and a pin of queue to NULL.
		{0x88, 3, 0, 3, 0, 0x82, 0, 0x80, 0,
			1, 3, 0, 0, 0, 0, 0, 1, 0, 0, 1, 0,
			1, 2, 0, 0, 0, 0, 4, 1, 5, 0, 0,
			1, 1, 0, 4, 0, 3, 0, 0, 0, 5,
			2, 0, 2, 1, 5, 0, 0, 0, 0, 0, 0, 0, 4, 7,
			11, 0, 4, 0, 0, 0, 0,
			1, 2, 0, 0, 0, 0, 0, 4, 0,
			4, 0, 0, 0, 1,
			8, 1, 7, 0, 2, 2, 4, 0, 5,
			1, 1, 0, 3, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0,
			1, 1, 0, 2, 0},
		// t (c0 int primary key, c1 text) with i0 (c1): the statements of slice 10, as [indexesCalls]
		// writes them: create index if not exists i0 (c0), which does nothing; create index if not
		// exists i3 (c1), which creates it; drop index if exists i0, which drops it; drop index if exists
		// i9 and t, which do nothing; drop table if exists i3, which does nothing; create table if not
		// exists t, which does nothing; drop table if exists t, which drops it and i3, and does nothing
		// the second time; create index if not exists i4 (c1), which finds no table; create table if not
		// exists t, which creates it again; an insert; and a select of the rows.
		{1, 0, 9, 0, 0, 1,
			102, 0, 0,
			38, 0, 1,
			37,
			101,
			109,
			229,
			166,
			165,
			165,
			38, 0, 1,
			166,
			2, 0, 0, 0, 1},
	}
	for typ := range byte(len(whereTypes)) {
		// t (c0 primary key) with i0 (c0 desc): a pin; a range; an insert of a key that exists; a
		// delete; a drop of i0; a pin of the primary key; a create of i3 (c0); and a select.
		seeds = append(seeds, []byte{0, typ, 0, 0, 0x80,
			1, 1, 0, 0, 2, 0,
			1, 0, 3, 0, 1, 0, 4,
			2, 0, 3,
			4, 0, 0, 0, 1,
			5,
			1, 1, 0, 0, 2, 0,
			6, 0, 0,
			0, 0, 7, 0, 2})
		// t (c0, c1 primary key, c2), of three types, with c0 and c2 taking NULL, i0 (c0 desc) and i1
		// (c2, c0) before the rows, and i2 (c1 desc, c0 desc, c2) after them: pins and ranges by each
		// index, an update of c2, a delete, a drop of i1, and a select by i2 after it.
		seeds = append(seeds, []byte{5, typ | 8, (typ + 1) % 5, (typ+2)%5 | 8, 2, 0, 0x80, 1, 2, 0, 0x82, 0x81, 0x80, 0,
			1, 1, 0, 0, 1, 15, 0, 0, 0, 6,
			9, 2, 1, 0, 0, 2, 1, 0, 3,
			17, 3, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0,
			19, 4, 3, 1, 0, 0, 2,
			4, 0, 8,
			13,
			9, 2, 0, 0, 1, 0, 0, 1, 0})
		// The tables of the two seeds above with values on the left of slice 12: a pin of the key; a
		// range of both bounds, and of the lower one; a select and a delete of a comparison; after a
		// drop of i0, a lower bound of the key; and on the table of three columns, a pin of c2 and a
		// lower bound of c0 by i1, and the pin and both bounds.
		seeds = append(seeds, []byte{0, typ, 0, 0, 0x80,
			1, 1, 2, 0, 2, 0,
			1, 0, 51, 0, 1, 0, 4,
			1, 0, 19, 0, 1, 0, 4,
			0, 0, 134, 0, 2,
			4, 0, 135, 0, 3,
			5,
			1, 0, 17, 0, 3})
		seeds = append(seeds, []byte{5, typ | 8, (typ + 1) % 5, (typ+2)%5 | 8, 2, 0, 0x80, 1, 2, 0, 0x82, 0x81, 0x80, 0,
			9, 1, 2, 0, 1, 17, 0, 2,
			9, 1, 2, 0, 1, 51, 0, 2, 0, 4})
	}
	return seeds
}

func TestFuzzIndexes_seeds(t *testing.T) {
	t.Run("should reach each kind of path, choice, and statement of indexes with the seeds of FuzzIndexes", func(t *testing.T) {
		y := tally{}
		for _, data := range indexesSeeds() {
			indexesRun(t, newMemoryInstance(t), data, y)
		}
		for _, kind := range []string{
			"path of an index with 1 pinned", "path of an index with 2 pinned", "path of an index with 3 pinned",
			"path of an index with 1 pinned and a range", "path with a range on a descending column", "path with a pin of NULL",
			"path of an index that holds every column", "chosen an index with 1 pinned", "chosen an index with 2 pinned",
			"chosen an index with 2 pinned and a range of a table of several indexes", "chosen a get of a table of several indexes",
			"create index that holds the primary key ok", "create index of a descending column ok", "create index of 3 columns ok",
			"drop index ok", "create index ok", "delete with WHERE ok, rows changed", "update with WHERE ok, rows changed",
			// The statements of slice 10, as [reachExists] counts them.
			"create table if not exists did its work", "create table if not exists did nothing: a table of the name exists",
			"drop table if exists did its work", "drop table if exists did nothing: no table of the name exists",
			"drop table if exists did nothing: no table of the name exists, and an index has it",
			"create index if not exists did its work", "create index if not exists did nothing: an index of the name exists",
			"create index if not exists no such table", "drop index if exists did its work",
			"drop index if exists did nothing: no index of the name exists", "drop index if exists did nothing: no index of the name exists, and a table has it",
			// The values on the left of slice 12.
			"chosen a get by a value on the left", "chosen the primary key with 0 pinned and a range by a value on the left",
			"chosen an index with 1 pinned and a range of a table of several indexes by a value on the left",
			"path of the primary key with 0 pinned and a range of a WHERE with a value on the left",
			"path of an index with 0 pinned and a range of a WHERE with a value on the left",
			"path of an index with 1 pinned and a range of a WHERE with a value on the left",
			"value on the left of < finds rows", "delete with a value on the left ok, rows changed",
		} {
			is.True(t, y["reached: "+kind] > 0, kind)
		}
		is.Equal(t, 0, y["reached: WHERE run by a sample of its paths"])
	})
}
