package engine_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// slow reports whether the tests run with -short, the race detector, or the address sanitizer, which
// make them several times slower: the tests of many sequences then run a tenth of them.
func slow() bool {
	return testing.Short() || raceEnabled || asanEnabled
}

// answer of a statement: what is compared between the engine and SQLite.
type answer struct {
	// err is SQLite's error as "code/extended: message", "not supported", "no statement", or "".
	err string
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

// run the statement of src, as sqlite3_prepare_v2 and sqlite3_step run it.
func (o *oracle) run(t testing.TB, src string) answer {
	t.Helper()
	s, _, err := o.c.PrepareFirst(src)
	if err != nil {
		return answer{err: sqliteText(t, err)}
	}
	if s == nil {
		return answer{err: "no statement"}
	}
	defer func() { _ = s.Finalize() }()
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
	}
	t.Fatalf("%q: the engine gives %v", src, err)
	return ""
}

// valueText of a value of the engine.
func valueText(t testing.TB, v *s4qlpb.Value) string {
	t.Helper()
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return fmt.Sprint(v.GetInteger())
	case s4qlpb.Value_Null_case:
		return "null"
	}
	t.Fatalf("the engine gives a value of kind %v", v.WhichKind())
	return ""
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
	// changes is SQLite's count of changes on the connection, as the results of the engine give it.
	changes int64
	// memory is the state of an engine on a database in memory, which has no core, or nil.
	memory func() string
	// coreLog is the logger of the core, or nil.
	coreLog *slog.Logger
}

// readers is the number of read connections of an instance.
const readers = 4

// newMemoryInstance of the engine on a database in memory.
func newMemoryInstance(t testing.TB) *instance {
	e, state := engine.NewMemory(sim.NewFree(sim.Buggify{}))
	return &instance{t: t, e: e, memory: state}
}

func newInstance(t testing.TB, options engine.Options) *instance {
	t.Helper()
	return newInstanceLogged(t, options, nil)
}

// newInstanceLogged is [newInstance] with a logger of the core.
func newInstanceLogged(t testing.TB, options engine.Options, coreLog *slog.Logger) *instance {
	t.Helper()
	h := sim.NewFree(sim.Buggify{})
	i := &instance{t: t, host: h, store: simstore.New(h, simstore.Options{}), dir: t.TempDir(), coreLog: coreLog}
	i.open(true, options)
	t.Cleanup(func() { i.close() })
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
	coreOptions := core.Options{Readers: readers, BusyTimeout: time.Minute, Lease: time.Hour, Logger: i.coreLog}
	d, err := core.Open(ctx, i.host, i.store.Client(fmt.Sprint("client-", i.host.Uint64()), nil), i.path, "db", coreOptions)
	is.NotError(i.t, err)
	is.NotError(i.t, d.Wait(ctx))
	i.d, i.e = d, engine.New(d, i.host, options)
}

// close the instance, which releases the lease.
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

// query src through [engine.Engine.Query].
func (i *instance) query(src string) answer {
	i.t.Helper()
	rows, err := i.e.Query(i.t.Context(), src)
	if rows != nil {
		if res := rows.Result(); res.SetsChanges {
			i.changes = res.Changes
		}
	}
	if err != nil {
		return answer{err: errText(i.t, src, err)}
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

// exec src through [engine.Engine.Exec], and keep SQLite's count of changes as its result says.
func (i *instance) exec(src string) answer {
	i.t.Helper()
	res, err := i.e.Exec(i.t.Context(), src)
	if res.SetsChanges {
		i.changes = res.Changes
	}
	if err != nil {
		return answer{err: errText(i.t, src, err)}
	}
	return answer{}
}

// state of the database in the engine's instance, as [oracle.state] gives it, read from the core.
func (i *instance) state() string {
	i.t.Helper()
	if i.memory != nil {
		return i.memory()
	}
	ctx := i.t.Context()
	r, err := i.d.Read(ctx)
	is.NotError(i.t, err)
	defer func() { is.NotError(i.t, r.Close(ctx)) }()
	tables, err := r.Tables(ctx)
	is.NotError(i.t, err)
	var b strings.Builder
	for _, ct := range tables {
		var columns []string
		for _, c := range ct.GetColumns() {
			columns = append(columns, c.GetName()+" INT")
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

// sequence of random statements of slice 1 that meet at a few tables and columns, each named in
// several spellings that SQLite takes for one name and in some that it takes for others, with
// values from a small set so that keys repeat, and with statements that are not supported and texts
// that SQLite refuses mixed in.
type sequence struct {
	r       stmttest.Rand
	g       *stmttest.Generator
	tables  []string
	columns []string
}

// spellings of the name raw: itself, and other spellings in and without quotes, of which those that
// change only ASCII letters name the same name, and an upper case that changes other letters names
// another.
func spellings(r stmttest.Rand, raw string) []string {
	if raw[0] == '"' {
		v := strings.ReplaceAll(raw[1:len(raw)-1], `""`, `"`)
		return []string{raw, stmttest.Quote(swapCase(v)), stmttest.Quote(strings.ToUpper(v)), stmttest.Quote(randomCase(r, v))}
	}
	return []string{raw, strings.ToUpper(raw), randomCase(r, raw), stmttest.Quote(raw), stmttest.Quote(randomCase(r, raw))}
}

// swapCase of the ASCII letters of s.
func swapCase(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case 'a' <= c && c <= 'z':
			b[i] = c - 'a' + 'A'
		case 'A' <= c && c <= 'Z':
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}

// randomCase of the ASCII letters of s.
func randomCase(r stmttest.Rand, s string) string {
	b := []byte(s)
	for i, c := range b {
		if r.IntN(2) == 0 {
			continue
		}
		switch {
		case 'a' <= c && c <= 'z':
			b[i] = c - 'a' + 'A'
		case 'A' <= c && c <= 'Z':
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}

// commonPools are the names that SQLite reads as a name of a table in every position of a table, and
// those that it reads as a name of a column in every position of a column.
var commonPools = sync.OnceValues(func() ([2][]string, error) {
	pools, err := stmttest.NamePools()
	if err != nil {
		return [2][]string{}, err
	}
	return [2][]string{
		common(pools, stmttest.CreateTable, stmttest.InsertTable, stmttest.SelectTable, stmttest.DropTable),
		common(pools, stmttest.CreateColumn, stmttest.InsertColumn),
	}, nil
})

// common names of the pools at the positions.
func common(pools [][]string, positions ...stmttest.Position) []string {
	var names []string
	for _, n := range pools[positions[0]] {
		in := true
		for _, p := range positions[1:] {
			in = in && slices.Contains(pools[p], n)
		}
		if in {
			names = append(names, n)
		}
	}
	return names
}

func newSequence(t testing.TB, r stmttest.Rand) *sequence {
	t.Helper()
	_, err := commonPools()
	is.NotError(t, err)
	return newSequenceFrom(r)
}

// newSequenceFrom r, after [commonPools] has succeeded once.
func newSequenceFrom(r stmttest.Rand) *sequence {
	q := &sequence{r: r}
	common, err := commonPools()
	if err != nil {
		panic(err)
	}
	tableNames, columnNames := common[0], common[1]
	for range 1 + r.IntN(2) {
		q.tables = append(q.tables, spellings(r, tableNames[r.IntN(len(tableNames))])...)
	}
	for range 1 + r.IntN(2) {
		q.columns = append(q.columns, spellings(r, columnNames[r.IntN(len(columnNames))])...)
	}
	pools := make([][]string, len(stmttest.PositionTexts))
	for _, p := range []stmttest.Position{stmttest.CreateTable, stmttest.InsertTable, stmttest.SelectTable, stmttest.DropTable} {
		pools[p] = q.tables
	}
	pools[stmttest.CreateColumn], pools[stmttest.InsertColumn] = q.columns, q.columns
	q.g = stmttest.NewGenerator(r, pools)
	q.g.Value = func() int64 {
		if r.IntN(4) == 0 {
			return stmttest.Integer(r)
		}
		return int64(r.IntN(5)) - 1
	}
	return q
}

// others are texts that are not statements of slice 1, with %[1]v for a table, %[2]v for a column,
// and %[3]v for a value: statements that the pass does not support, texts that SQLite refuses with a
// syntax error, and texts without a statement.
var others = []string{
	"create table %[1]v (%[2]v integer primary key) strict, without rowid",
	"create table %[1]v (%[2]v int primary key) strict",
	"create table if not exists %[1]v (%[2]v int primary key) strict, without rowid",
	`create table "" (%[2]v int primary key) strict, without rowid`,
	`create table %[1]v ("" int primary key) strict, without rowid`,
	"insert into %[1]v (%[2]v) values (%[3]v), (%[3]v)",
	"insert or replace into %[1]v (%[2]v) values (%[3]v)",
	"insert into %[1]v (%[2]v) values (+%[3]v)",
	"insert into %[1]v values (%[3]v)",
	`insert into "" (%[2]v) values (%[3]v)`,
	"select * from %[1]v where %[2]v = %[3]v",
	"select %[2]v from %[1]v",
	`select * from ""`,
	"drop table if exists %[1]v",
	"update %[1]v set %[2]v = %[3]v",
	"begin",
	"select * from %[1]v; select * from %[1]v",
	"select * frm %[1]v",
	"insert into %[1]v (%[2]v) values (",
	"create table %[1]v (%[2]v int primary key) strict without rowid",
	"drop table",
	"",
	" ; -- nothing",
}

// next statement of the sequence, whether to run it as a query, and whether it is a statement of
// slice 1, which the engine must support.
func (q *sequence) next() (src string, query, supported bool) {
	r := q.r
	if r.IntN(12) == 0 {
		form := others[r.IntN(len(others))]
		table, column, value := q.tables[r.IntN(len(q.tables))], q.columns[r.IntN(len(q.columns))], r.IntN(5)
		query := r.IntN(2) == 0
		if !strings.Contains(form, "%") {
			// Sprintf would add the arguments that a form without a verb does not use to its text.
			return form, query, false
		}
		return fmt.Sprintf(form, table, column, value), query, false
	}
	var k stmttest.Kind
	switch n := r.IntN(20); {
	case n < 4:
		k = stmttest.KindCreateTable
	case n < 14:
		k = stmttest.KindInsert
	case n < 19:
		k = stmttest.KindSelect
	default:
		k = stmttest.KindDropTable
	}
	src, _ = q.g.StatementOf(k)
	// A query runs through Exec one time in eight, and every other statement through Query one time
	// in eight.
	return src, (k == stmttest.KindSelect) != (r.IntN(8) == 0), true
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

// knownGap of the pass after the parser, which its README describes and its tests excuse in the same
// way, that the engine's answer got and SQLite's answer want for the text src fall in, or "". The
// engine gives a syntax error, and SQLite another error with the codes 1 and 1, of one of its
// grammar actions:
//
//   - SQLite does not say whether src parses, because the error of the action stopped it before the
//     end of the statement, also on an empty database, such as `unknown table option`.
//   - SQLite gives the engine's syntax error for src on a database with the tables that src names,
//     where [sqloracle.Judge] runs it, and the error of the action is about what the database holds,
//     such as `no such table: main.X` for a trigger, which SQLite gives on an empty database too.
func knownGap(src string, got, want answer) string {
	prefix := fmt.Sprint(sqlerr.CodeError, "/", sqlerr.CodeError, ": ")
	m, ok := strings.CutPrefix(got.err, prefix)
	if !ok || !sqloracle.IsSyntaxMessage(m) {
		return ""
	}
	if w, ok := strings.CutPrefix(want.err, prefix); !ok || sqloracle.IsSyntaxMessage(w) {
		return ""
	}
	switch v := sqloracle.Judge(src); {
	case v.Kind == sqloracle.NotSaid && v.Reason != sqloracle.Unsettled && v.Reason != sqloracle.NotParsed:
		return "syntax error where SQLite does not say"
	case v.Kind == sqloracle.SyntaxError && v.Message == m:
		return "error about the database before a syntax error"
	}
	return ""
}

// startedTable reports whether the engine's answer got and SQLite's answer want for the text src
// differ in the one way that the tests accept. The text is a syntax error, for which the engine gives
// the error that SQLite gives on an empty database, and SQLite gives `table %s already exists`, with
// the name as written, for the table that [stmttest.StartedTable] says the text starts. The spec:
// "Text with a syntax error gets the error that SQLite gives for it on an empty database."
func startedTable(t testing.TB, empty *oracle, src string, got, want answer) bool {
	t.Helper()
	_, err := stmt.Check(src)
	var se *sqlerr.Error
	if !errors.As(err, &se) {
		return false
	}
	n, ok := stmttest.StartedTable(src, se)
	if !ok || want.err != fmt.Sprint(sqlerr.CodeError, "/", sqlerr.CodeError, ": table ", n.Raw, " already exists") {
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
		src, query, supported := q.next()
		log = append(log, src)
		if failure := p.step(src, query, supported); failure != "" {
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

// step runs src on the engine, through Query if query and through Exec otherwise, and on SQLite,
// and compares what each gives, the count of changes, and the last insert rowid. A text that the
// engine does not support is not run in SQLite: it must not be a statement of slice 1, which
// supported says it is, and SQLite must prepare it on an empty database without a syntax error. step
// gives what differs, or "".
func (p *pair) step(src string, query, supported bool) string {
	t, i, o := p.t, p.i, p.o
	t.Helper()
	var got answer
	if query {
		got = i.query(src)
	} else {
		got = i.exec(src)
	}
	p.y.add(src, got)
	if got.err == "not supported" {
		// A statement of slice 1 is supported, and a text that is not supported is one that SQLite
		// accepts: SQLite prepares it on an empty database without a syntax error.
		if supported {
			return fmt.Sprintf("%q: the engine does not support a statement of slice 1", src)
		}
		// A text with a NUL byte or a byte that is not UTF-8 is not supported, whatever else it holds.
		if !utf8.ValidString(src) || strings.ContainsRune(src, 0) {
			return ""
		}
		if err := p.empty.prepare(t, src); err != nil && sqloracle.IsSyntaxMessage(err.Message) {
			return fmt.Sprintf("%q: the engine does not support it, and SQLite gives %v", src, err.Message)
		}
		return ""
	}
	before := p.prev
	want := o.run(t, src)
	if !query {
		want.columns, want.rows = "", nil
	}
	if got.String() != want.String() {
		gap := knownGap(src, got, want)
		switch {
		case startedTable(t, p.empty, src, got, want):
			p.y.add("", answer{err: "already exists after the start of a table, accepted"})
		case gap != "":
			p.y.add("", answer{err: gap + ", a known gap"})
		default:
			return fmt.Sprintf("%q: the engine gives\n%v\nand SQLite\n%v", src, got, want)
		}
	}
	// SQLite's count of changes: the engine's result sets it, and a statement that fails keeps it,
	// except one that fails while SQLite steps it, a broken constraint, which sets it to 0.
	changes, rowid := o.changes(t)
	p.prev = changes
	switch {
	case strings.HasPrefix(want.err, fmt.Sprint(sqlerr.CodeConstraint, "/")):
		if changes != 0 {
			return fmt.Sprintf("%q: SQLite counts %v changes after a broken constraint", src, changes)
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
// [TestDifferential], the texts of [others] and [fuzzSetUp], and every 500th text of the corpus.
func fuzzSQL(f *testing.F, newInstance func(t testing.TB) *instance) {
	q := newSequence(f, rand.New(rand.NewPCG(1, 2)))
	for range 40 {
		src, query, _ := q.next()
		f.Add(src, !query)
	}
	for _, src := range slices.Concat(fuzzSetUp, others) {
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
			if failure := p.step(s, false, true); failure != "" {
				t.Fatalf("the set-up: %v", failure)
			}
		}
		if failure := p.step(src, !exec, false); failure != "" {
			t.Fatal(failure)
		}
		if got, want := i.state(), p.o.state(t); got != want {
			t.Fatalf("%q: the engine holds\n%v\nand SQLite\n%v", src, got, want)
		}
	})
}
