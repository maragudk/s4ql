package engine_test

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"

	"hegel.dev/go/hegel"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

// The property-based check of the engine with Hegel (hegel.dev). It draws a short series of
// statements on two tables, runs it on the engine and on SQLite through [pair.step], as
// [TestDifferential] does with the sequences of [stmttest.Sequence], and requires the same answers
// and, at the end, the same tables and rows. Where a series fails, Hegel shrinks it, by fewer
// statements and simpler ones, before it reports it, and it reports the draws of the series.

// hegelOptions of the property-based check: the same series on every run and every machine, no
// directory of failing examples written beside the package, and no check of how fast a series runs,
// which depends on the machine.
func hegelOptions(cases int) []hegel.Option {
	return []hegel.Option{
		hegel.WithTestCases(cases),
		hegel.WithDerandomize(true),
		hegel.WithDatabase(""),
		hegel.SuppressHealthCheck(hegel.TooSlow),
	}
}

// TestHegel_series checks the engine against SQLite with the series of statements that [runSeries]
// draws.
func TestHegel_series(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		if failure := runSeries(t, ht, engine.Options{}); failure != "" {
			ht.Fatal(failure)
		}
	}, hegelOptions(200)...)
}

// runSeries draws a series of statements and runs it on a pair of the engine with options and SQLite,
// with the length limit and the restarts of the instance that it draws, and gives the first difference,
// with the statements before it, or "". The instances and the oracles are of a [seriesT] of the
// series, which closes them at its end.
func runSeries(t *testing.T, tc hegel.TestCase, options engine.Options) (failure string) {
	ct := &seriesT{T: t, tc: tc}
	defer func() {
		ct.close()
		if failure == "" && len(ct.errors) > 0 {
			failure = strings.Join(ct.errors, "\n")
		}
	}()
	// The directions in which the queries of both instances read, which the flag -directions sets for
	// every instance, are drawn too, unless the flag gives them, so that a series is the same each time
	// that Hegel runs it.
	if *directions == 0 {
		defer func() { *directions = 0 }()
		*directions = hegel.Draw(tc, hegel.Integers[uint64](1, math.MaxUint64))
	}
	// The length limit of the cache and of SQLite, SQLite's default or [stmttest.LengthLimit], as
	// [variantOf] gives it to the sequences of TestDifferential.
	limit := 0
	if hegel.Draw(tc, hegel.WeightedBooleans(0.25)) {
		limit = stmttest.LengthLimit
	}
	i := newInstanceLimited(ct, options, nil, limit)
	i.planted = options != engine.Options{}
	p := newPairWith(ct, i, nil, options)
	p.rnd = rand.New(rand.NewPCG(1, 0x9a7))
	s := &series{tc: tc, shapes: map[string][]seriesColumn{}}
	var log []string
	fail := func(format string, args ...any) string {
		return fmt.Sprintf("after\n%v\n%v", strings.Join(log, "\n"), fmt.Sprintf(format, args...))
	}
	for range hegel.Draw(tc, hegel.Integers(1, 30)) {
		if hegel.Draw(tc, hegel.WeightedBooleans(0.05)) {
			empty := hegel.Draw(tc, hegel.Booleans())
			log = append(log, fmt.Sprint("-- restart, with an empty cache: ", empty))
			tc.Note(log[len(log)-1])
			i.restart(empty, options)
			if got, want := i.state(), p.o.state(ct); got != want {
				return fail("after the restart the engine holds\n%v\nand SQLite\n%v", got, want)
			}
			continue
		}
		c := s.next()
		log = append(log, fmt.Sprintf("%v -- with %#v", c.Src, c.Args))
		tc.Note(log[len(log)-1])
		if failure := p.step(c); failure != "" {
			return fail("%v", failure)
		}
		tc.Event(seriesOutcome(c.Src, p.last))
	}
	want := p.o.state(ct)
	if got := i.state(); got != want {
		return fail("at the end the engine holds\n%v\nand SQLite\n%v", got, want)
	}
	if got := p.other.state(); got != want {
		return fail("at the end the other engine, which reads the rows the other way, holds\n%v\nand SQLite\n%v", got, want)
	}
	return ""
}

// seriesT is the [testing.TB] of a series: the test that runs Hegel, with the cleanups and the
// temporary directories of the series alone, which [seriesT.close] runs and removes at its end, so
// that thousands of series in a run, or in a shrinking, do not keep their instances open. A fatal
// error of a helper panics, which ends the series as a failure that Hegel shrinks, where Goexit on
// the test would end the run; an error that is not fatal is kept and fails the series at its end. The
// logs of the helpers are notes, which Hegel prints for the shrunk series alone.
type seriesT struct {
	*testing.T
	tc       hegel.TestCase
	cleanups []func()
	dirs     []string
	errors   []string
	failed   bool
}

func (c *seriesT) Cleanup(f func()) { c.cleanups = append(c.cleanups, f) }

func (c *seriesT) Error(args ...any) {
	c.failed = true
	c.errors = append(c.errors, fmt.Sprint(args...))
}

func (c *seriesT) Errorf(format string, args ...any) { c.Error(fmt.Sprintf(format, args...)) }

func (c *seriesT) Fail() { c.failed = true }

func (c *seriesT) FailNow() { c.Fatal("FailNow") }

func (c *seriesT) Failed() bool { return c.failed }

func (c *seriesT) Fatal(args ...any) {
	c.failed = true
	panic(fmt.Sprint(args...))
}

func (c *seriesT) Fatalf(format string, args ...any) { c.Fatal(fmt.Sprintf(format, args...)) }

func (c *seriesT) Log(args ...any) { c.tc.Note(fmt.Sprint(args...)) }

func (c *seriesT) Logf(format string, args ...any) { c.Log(fmt.Sprintf(format, args...)) }

func (c *seriesT) TempDir() string {
	dir, err := os.MkdirTemp("", "series")
	if err != nil {
		c.Fatal(err)
	}
	c.dirs = append(c.dirs, dir)
	return dir
}

// close runs the cleanups of the series, the last first, and removes its temporary directories.
func (c *seriesT) close() {
	defer func() {
		for _, dir := range c.dirs {
			if err := os.RemoveAll(dir); err != nil {
				c.Error(err)
			}
		}
	}()
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		c.cleanups[i]()
	}
}

// seriesOutcome of the statement src with the engine's answer a, for the statistics of a run: the kind of
// the statement, and its error, or whether it gave rows.
func seriesOutcome(src string, a answer) string {
	kind, _, _ := strings.Cut(src, " ")
	if strings.Contains(src, " returning ") {
		kind += " returning"
	}
	switch {
	case a.err != "":
		code, _, _ := strings.Cut(a.err, ":")
		return kind + " error " + code
	case len(a.rows) > 0:
		return kind + " with rows"
	}
	return kind + " ok"
}

// seriesTables are the names of the tables of a series, and seriesIndexes those of its indexes.
var (
	seriesTables  = []string{"t", "u"}
	seriesIndexes = []string{"i1", "i2", "i3"}
)

// seriesColumn of a table of a series: its name, its declared type, whether it takes NULL, and the
// literal of its default, or "".
type seriesColumn struct {
	name, typ string
	nullable  bool
	dflt      string
}

// series of statements that Hegel draws, with what it created: the shapes of the tables that it
// created and did not drop since, by name, whether SQLite took the statements or not.
type series struct {
	tc     hegel.TestCase
	shapes map[string][]seriesColumn
	b      strings.Builder
	args   []any
}

// next statement of the series: a create table where no table is there and one time in twelve
// otherwise, and otherwise a statement of every kind, inserts and selects the most, of a table that
// is there nine times in ten.
func (s *series) next() stmttest.Call {
	s.b.Reset()
	s.args = nil
	if len(s.shapes) == 0 || hegel.Draw(s.tc, hegel.WeightedBooleans(0.08)) {
		s.createTable()
		return s.call(false)
	}
	table := s.table()
	switch hegel.Draw(s.tc, hegel.Integers(0, 12)) {
	case 0:
		s.dropTable(table)
	case 1:
		s.createIndex(table)
	case 2:
		s.dropIndex()
	case 3, 4, 5, 6:
		return s.call(s.insert(table))
	case 7, 8:
		return s.call(s.update(table))
	case 9:
		return s.call(s.delete(table))
	default:
		s.selectFrom(table)
		return s.call(true)
	}
	return s.call(false)
}

// call of the statement that the series wrote, as a query if query.
func (s *series) call(query bool) stmttest.Call {
	return stmttest.Call{Src: s.b.String(), Args: s.args, Query: query, Supported: true}
}

// table of a statement: one that is there nine times in ten, and otherwise any name.
func (s *series) table() string {
	var live []string
	for _, name := range seriesTables {
		if s.shapes[name] != nil {
			live = append(live, name)
		}
	}
	if len(live) == 0 || hegel.Draw(s.tc, hegel.WeightedBooleans(0.1)) {
		return hegel.Draw(s.tc, hegel.SampledFrom(seriesTables))
	}
	return hegel.Draw(s.tc, hegel.SampledFrom(live))
}

// columns of the table, as the series knows them, or nil for a table that is not there.
func (s *series) columns(table string) []seriesColumn {
	return s.shapes[table]
}

// createTable writes a create table of a table of a key and one to three other columns, the key at
// any place, each of the types of s4ql, NOT NULL and with a default now and then.
func (s *series) createTable() {
	table := hegel.Draw(s.tc, hegel.SampledFrom(seriesTables))
	n := hegel.Draw(s.tc, hegel.Integers(2, 4))
	key := hegel.Draw(s.tc, hegel.Integers(0, n-1))
	var columns []seriesColumn
	var texts []string
	for i := range n {
		c := seriesColumn{name: fmt.Sprint("c", i), typ: hegel.Draw(s.tc, hegel.SampledFrom([]string{"int", "integer", "real", "text", "blob"}))}
		text := c.name + " " + c.typ
		if i == key {
			c.name = "k"
			text = "k " + c.typ + " primary key"
		} else {
			c.nullable = !hegel.Draw(s.tc, hegel.WeightedBooleans(0.3))
			if !c.nullable {
				text += " not null"
			}
		}
		if hegel.Draw(s.tc, hegel.WeightedBooleans(0.3)) {
			c.dflt = s.literal(c.typ)
			text += " default " + c.dflt
		}
		columns = append(columns, c)
		texts = append(texts, text)
	}
	s.b.WriteString("create table ")
	if hegel.Draw(s.tc, hegel.WeightedBooleans(0.2)) {
		s.b.WriteString("if not exists ")
	}
	// A table that is there keeps its shape: SQLite refuses the statement, or does nothing.
	if s.shapes[table] == nil {
		s.shapes[table] = columns
	}
	fmt.Fprintf(&s.b, "%v(%v) strict, without rowid", table, strings.Join(texts, ", "))
}

// dropTable writes a drop table of the table.
func (s *series) dropTable(table string) {
	s.b.WriteString("drop table ")
	if hegel.Draw(s.tc, hegel.WeightedBooleans(0.2)) {
		s.b.WriteString("if exists ")
	}
	s.b.WriteString(table)
	delete(s.shapes, table)
}

// createIndex writes a create index of one or two columns of the table, each ascending or
// descending, and none twice, which s4ql refuses.
func (s *series) createIndex(table string) {
	columns := s.columns(table)
	if columns == nil {
		columns = []seriesColumn{{name: "c0"}}
	}
	var keys []string
	for _, i := range hegel.Draw(s.tc, hegel.UniqueLists(hegel.Integers(0, len(columns)-1)).MinSize(1).MaxSize(2)) {
		direction := hegel.Draw(s.tc, hegel.SampledFrom([]string{"", " asc", " desc"}))
		keys = append(keys, columns[i].name+direction)
	}
	s.b.WriteString("create index ")
	if hegel.Draw(s.tc, hegel.WeightedBooleans(0.2)) {
		s.b.WriteString("if not exists ")
	}
	fmt.Fprintf(&s.b, "%v on %v(%v)", hegel.Draw(s.tc, hegel.SampledFrom(seriesIndexes)), table, strings.Join(keys, ", "))
}

// dropIndex writes a drop index.
func (s *series) dropIndex() {
	s.b.WriteString("drop index ")
	if hegel.Draw(s.tc, hegel.WeightedBooleans(0.2)) {
		s.b.WriteString("if exists ")
	}
	s.b.WriteString(hegel.Draw(s.tc, hegel.SampledFrom(seriesIndexes)))
}

// insert writes an insert of one row into the table, of every column in the order of the table, or
// one time in two of a list of its columns in any order, which may leave columns out, with a
// RETURNING one time in four, and reports whether it has one.
func (s *series) insert(table string) bool {
	columns := s.columns(table)
	if columns == nil {
		columns = []seriesColumn{{name: "c0", typ: "int"}}
	}
	fmt.Fprintf(&s.b, "insert into %v", table)
	listed := columns
	if hegel.Draw(s.tc, hegel.Booleans()) {
		listed = nil
		order := hegel.Draw(s.tc, hegel.UniqueLists(hegel.Integers(0, len(columns)-1)).MinSize(1).MaxSize(len(columns)))
		var names []string
		for _, i := range order {
			listed = append(listed, columns[i])
			names = append(names, columns[i].name)
		}
		fmt.Fprintf(&s.b, "(%v)", strings.Join(names, ", "))
	}
	var values []string
	for _, c := range listed {
		values = append(values, s.value(c.typ, c.nullable))
	}
	fmt.Fprintf(&s.b, " values (%v)", strings.Join(values, ", "))
	return s.returning(columns)
}

// update writes an update of one or two columns of the table, with a WHERE three times in four, and
// a RETURNING one time in four, and reports whether it has one.
func (s *series) update(table string) bool {
	columns := s.columns(table)
	if columns == nil {
		columns = []seriesColumn{{name: "c0", typ: "int"}}
	}
	var sets []string
	for _, i := range hegel.Draw(s.tc, hegel.UniqueLists(hegel.Integers(0, len(columns)-1)).MinSize(1).MaxSize(2)) {
		sets = append(sets, columns[i].name+" = "+s.value(columns[i].typ, columns[i].nullable))
	}
	fmt.Fprintf(&s.b, "update %v set %v", table, strings.Join(sets, ", "))
	s.where(columns)
	return s.returning(columns)
}

// delete writes a delete from the table, with a WHERE three times in four, and a RETURNING one time
// in four, and reports whether it has one.
func (s *series) delete(table string) bool {
	columns := s.columns(table)
	fmt.Fprintf(&s.b, "delete from %v", table)
	s.where(columns)
	return s.returning(columns)
}

// selectFrom writes a select of the table: of *, or of one to three of its columns, with a WHERE three
// times in four, an ORDER BY of one to three columns, none twice, one time in two, and a LIMIT one
// time in three.
func (s *series) selectFrom(table string) {
	columns := s.columns(table)
	s.b.WriteString("select ")
	s.list(columns)
	fmt.Fprintf(&s.b, " from %v", table)
	s.where(columns)
	if len(columns) > 0 && hegel.Draw(s.tc, hegel.Booleans()) {
		var keys []string
		for _, i := range hegel.Draw(s.tc, hegel.UniqueLists(hegel.Integers(0, len(columns)-1)).MinSize(1).MaxSize(3)) {
			direction := hegel.Draw(s.tc, hegel.SampledFrom([]string{"", " asc", " desc"}))
			keys = append(keys, columns[i].name+direction)
		}
		fmt.Fprintf(&s.b, " order by %v", strings.Join(keys, ", "))
	}
	if hegel.Draw(s.tc, hegel.WeightedBooleans(1.0/3)) {
		n := hegel.Draw(s.tc, hegel.Integers[int64](0, 4))
		if hegel.Draw(s.tc, hegel.WeightedBooleans(0.3)) {
			s.b.WriteString(" limit ?")
			s.args = append(s.args, n)
		} else {
			fmt.Fprintf(&s.b, " limit %v", n)
		}
	}
}

// list writes * one time in three, and otherwise one to three of columns, a column maybe twice.
func (s *series) list(columns []seriesColumn) {
	if len(columns) == 0 || hegel.Draw(s.tc, hegel.WeightedBooleans(1.0/3)) {
		s.b.WriteString("*")
		return
	}
	var names []string
	for range hegel.Draw(s.tc, hegel.Integers(1, 3)) {
		names = append(names, hegel.Draw(s.tc, hegel.SampledFrom(columns)).name)
	}
	s.b.WriteString(strings.Join(names, ", "))
}

// where writes a WHERE three times in four of one to three conditions joined by AND: a comparison of
// a column with a value, the value on the left one time in five, or IS NULL or IS NOT NULL.
func (s *series) where(columns []seriesColumn) {
	if len(columns) == 0 || hegel.Draw(s.tc, hegel.WeightedBooleans(0.25)) {
		return
	}
	var conditions []string
	for range hegel.Draw(s.tc, hegel.Integers(1, 3)) {
		c := hegel.Draw(s.tc, hegel.SampledFrom(columns))
		if hegel.Draw(s.tc, hegel.WeightedBooleans(0.1)) {
			conditions = append(conditions, c.name+hegel.Draw(s.tc, hegel.SampledFrom([]string{" is null", " is not null"})))
			continue
		}
		op := hegel.Draw(s.tc, hegel.SampledFrom([]string{"=", "<", "<=", ">", ">="}))
		v := s.value(c.typ, false)
		if hegel.Draw(s.tc, hegel.WeightedBooleans(0.2)) {
			conditions = append(conditions, v+" "+op+" "+c.name)
		} else {
			conditions = append(conditions, c.name+" "+op+" "+v)
		}
	}
	fmt.Fprintf(&s.b, " where %v", strings.Join(conditions, " and "))
}

// returning writes a RETURNING one time in four, of * or of columns as [series.list] writes them,
// and reports whether it wrote one.
func (s *series) returning(columns []seriesColumn) bool {
	if !hegel.Draw(s.tc, hegel.WeightedBooleans(0.25)) {
		return false
	}
	s.b.WriteString(" returning ")
	s.list(columns)
	return true
}

// value of a column of the type typ: NULL one time in ten if nullable, and otherwise a value of the
// type, one time in 100 a value of another type, which s4ql refuses, as a literal, or as a parameter one time in three.
func (s *series) value(typ string, nullable bool) string {
	if nullable && hegel.Draw(s.tc, hegel.WeightedBooleans(0.1)) {
		if hegel.Draw(s.tc, hegel.Booleans()) {
			s.args = append(s.args, nil)
			return "?"
		}
		return "null"
	}
	if hegel.Draw(s.tc, hegel.WeightedBooleans(0.01)) {
		typ = hegel.Draw(s.tc, hegel.SampledFrom([]string{"int", "real", "text", "blob"}))
	}
	if hegel.Draw(s.tc, hegel.WeightedBooleans(1.0/3)) {
		s.args = append(s.args, s.argument(typ))
		return "?"
	}
	return s.literal(typ)
}

// argument of the type typ: an int64, a float64, a string, or a []byte, three times in four from a
// few values each, so that keys and values meet, and otherwise any value that a column of the type
// holds: a finite real, and a text without NUL.
func (s *series) argument(typ string) any {
	few := hegel.Draw(s.tc, hegel.WeightedBooleans(0.75))
	switch typ {
	case "int", "integer":
		if few {
			return hegel.Draw(s.tc, hegel.OneOf(hegel.Integers[int64](-2, 3), hegel.SampledFrom([]int64{math.MinInt64, math.MaxInt64})))
		}
		return hegel.Draw(s.tc, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
	case "real":
		if few {
			return hegel.Draw(s.tc, hegel.SampledFrom([]float64{0, math.Copysign(0, -1), 0.5, -1.5, 2, 1e300}))
		}
		return hegel.Draw(s.tc, hegel.Floats[float64]().AllowNaN(false).AllowInfinity(false))
	case "text":
		if few {
			return hegel.Draw(s.tc, hegel.SampledFrom([]string{"", "a", "b", "B", "ab", "é"}))
		}
		return hegel.Draw(s.tc, hegel.Text().ExcludeCharacters("\x00").MaxSize(8))
	}
	if few {
		return hegel.Draw(s.tc, hegel.SampledFrom([][]byte{{}, {0}, {1}, {0xff}, {0, 1}}))
	}
	return hegel.Draw(s.tc, hegel.Binary(0, 8))
}

// literal of a value of the type typ, as [series.argument] draws it.
func (s *series) literal(typ string) string {
	switch v := s.argument(typ).(type) {
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		text := strconv.FormatFloat(v, 'g', -1, 64)
		if !strings.ContainsAny(text, ".e") {
			text += ".0"
		}
		return text
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case []byte:
		return "x'" + hex.EncodeToString(v) + "'"
	}
	panic("no literal")
}
