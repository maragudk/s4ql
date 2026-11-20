package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// fake storage: tables in memory, with names compared as SQLite compares them, and failures on
// demand. It is a [database], and each of its transactions sees and changes the same tables. It is
// the [readConn] of each of its readers too, which it only counts: a read on it does not fail while
// the one before it is open.
type fake struct {
	tables map[string]*fakeTable
	// scanFails makes a scan fail before the row of that index, if it is not negative.
	scanFails int
	// insertFails, annotateFails, and commitFails are the errors of those calls. insertFails is the
	// error of an insert of a row also when the table has its key, as the core's too big is.
	insertFails, annotateFails, commitFails, beginFails, readFails error
	// insertPanics makes an insert panic, and endPanics the end of a read transaction.
	insertPanics, endPanics bool
	// scanPanics makes a scan panic before the row of that index, if it is not negative.
	scanPanics int
	// tablePanics makes a lookup panic, tableFails is the error of a lookup, of the name tableFailsFor
	// only if that is not empty, and endFails is the error of the end of a read transaction.
	tablePanics   bool
	tableFails    error
	tableFailsFor string
	endFails      error
	// scans counts the scans that have started and not ended, and open the transactions.
	scans, open int
	// scanned counts the scans that have started, getFails is the error of a get, and deleteFails of
	// a delete.
	scanned               int
	getFails, deleteFails error
	// addFails is the error of an add of a key to a set, and readKeysFails of the read of the keys of
	// a set, before its first key.
	addFails, readKeysFails error
	// indexesFails is the error of the read of the indexes of a table, and indexFails of the lookup
	// of an index by its name.
	indexesFails, indexFails error
	// readers counts the readers that are open, and opened those that were ever opened. readerFails
	// is the error of opening one, and closeFails of closing one.
	readers, opened         int
	readerFails, closeFails error
	// calls of the transactions, in order.
	calls []string
	// rowsGiven counts the rows that the scans and the gets gave.
	rowsGiven int64
}

type fakeTable struct {
	ct   *s4qlpb.CreateTable
	rows [][]*s4qlpb.Value
	// indexes of the table, in the order in which they were created.
	indexes []*s4qlpb.CreateIndex
}

func newFake() *fake {
	return &fake{tables: map[string]*fakeTable{}, scanFails: -1, scanPanics: -1}
}

// with a table of the name and the columns, of which the first is the primary key, and rows of
// integers.
func (f *fake) with(name string, columns []string, rows ...[]int64) *fake {
	ct := s4qlpb.CreateTable_builder{Table: new(name), PrimaryKey: []uint32{0}}
	for _, c := range columns {
		ct.Columns = append(ct.Columns, s4qlpb.Column_builder{Name: new(c), Type: new(s4qlpb.Type_TYPE_INT)}.Build())
	}
	t := &fakeTable{ct: ct.Build()}
	for _, r := range rows {
		row := make([]*s4qlpb.Value, len(r))
		for i, v := range r {
			row[i] = s4qlpb.Value_builder{Integer: new(v)}.Build()
		}
		t.rows = append(t.rows, row)
	}
	f.tables[fold(name)] = t
	return f
}

func (f *fake) begin(context.Context) (writeTx, error) {
	if f.beginFails != nil {
		return nil, f.beginFails
	}
	f.open++
	return fakeTx{f}, nil
}

// lengthLimit of the fake is SQLite's default.
func (f *fake) lengthLimit() int {
	return 1_000_000_000
}

func (f *fake) reader(context.Context) (readConn, error) {
	if f.readerFails != nil {
		return nil, f.readerFails
	}
	f.readers++
	f.opened++
	return f, nil
}

func (f *fake) close(context.Context) error {
	if errors.Is(f.closeFails, core.ErrReaderInUse) {
		return f.closeFails
	}
	f.readers--
	return f.closeFails
}

func (f *fake) read(context.Context) (readTx, error) {
	if f.readFails != nil {
		return nil, f.readFails
	}
	f.open++
	return fakeTx{f}, nil
}

// fakeTx is a transaction of a fake, read or write.
type fakeTx struct {
	f *fake
}

func (x fakeTx) table(_ context.Context, name string) (*s4qlpb.CreateTable, error) {
	x.f.calls = append(x.f.calls, "table "+name)
	if x.f.tablePanics {
		panic("the lookup panics")
	}
	if x.f.tableFails != nil && (x.f.tableFailsFor == "" || fold(x.f.tableFailsFor) == fold(name)) {
		return nil, x.f.tableFails
	}
	if t, ok := x.f.tables[fold(name)]; ok {
		return t.ct, nil
	}
	return nil, nil
}

// indexes records its call, as "indexes" and the name, and gives the indexes of the table.
func (x fakeTx) indexes(_ context.Context, name string) ([]*s4qlpb.CreateIndex, error) {
	x.f.calls = append(x.f.calls, "indexes "+name)
	if x.f.indexesFails != nil {
		return nil, x.f.indexesFails
	}
	return x.f.tables[fold(name)].indexes, nil
}

func (x fakeTx) index(_ context.Context, name string) (*s4qlpb.CreateIndex, error) {
	if x.f.indexFails != nil {
		return nil, x.f.indexFails
	}
	for _, t := range x.f.tables {
		for _, ix := range t.indexes {
			if fold(ix.GetIndex()) == fold(name) {
				return ix, nil
			}
		}
	}
	return nil, nil
}

func (x fakeTx) rowsRead() int64 {
	return x.f.rowsGiven
}

// with an index of the name on the columns at the positions of the table, each in ascending order,
// or in descending order for a position that is given as its negative minus one, as ^position is.
func (f *fake) index(name, table string, positions ...int) *fake {
	ci := s4qlpb.CreateIndex_builder{Index: new(name), Table: new(table)}
	for _, position := range positions {
		column := s4qlpb.IndexColumn_builder{Column: new(uint32(position))}
		if position < 0 {
			column = s4qlpb.IndexColumn_builder{Column: new(uint32(^position)), Descending: new(true)}
		}
		ci.Columns = append(ci.Columns, column.Build())
	}
	t := f.tables[fold(table)]
	t.indexes = append(t.indexes, ci.Build())
	return f
}

// scan the rows of the table, whose values are integers, as s says: in the order of the key of the
// index of s, or of the primary key, or its reverse, those whose leading columns are equal to the
// pinned values and whose next column is within the bounds. A scan that pins or bounds something, or
// names an index, records its call: "range" for one by the primary key, or "index" and the name of
// the index, then the name of the table, "= " and the value for each pinned column, and each bound
// as an operator and an integer. A scan backward records "backward" last. A scan of every row
// forward records nothing.
func (x fakeTx) scan(_ context.Context, name string, s core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error] {
	t := x.f.tables[fold(name)]
	var key []int
	descending := map[int]bool{}
	call := "range " + name
	if s.Index != "" {
		call = "index " + s.Index + " " + name
		for _, ix := range t.indexes {
			if fold(ix.GetIndex()) != fold(s.Index) {
				continue
			}
			for _, c := range ix.GetColumns() {
				key = append(key, int(c.GetColumn()))
				descending[int(c.GetColumn())] = c.GetDescending()
			}
		}
		if key == nil {
			panic("the fake has no index " + s.Index + " on " + name)
		}
	}
	for _, position := range t.ct.GetPrimaryKey() {
		if !slices.Contains(key, int(position)) {
			key = append(key, int(position))
		}
	}
	for _, v := range s.Equal {
		call += " = " + fmt.Sprint(v.GetInteger())
	}
	for _, b := range []struct {
		core.Bound
		op string
	}{{s.Lower, ">"}, {s.Upper, "<"}} {
		if b.Value != nil {
			call += " " + b.op + map[bool]string{true: "", false: "="}[b.Open] + " " + fmt.Sprint(b.Value.GetInteger())
		}
	}
	if s.Backward {
		call += " backward"
	}
	if call != "range "+name {
		x.f.calls = append(x.f.calls, call)
	}
	within := func(row []*s4qlpb.Value) bool {
		for i, v := range s.Equal {
			if row[key[i]].GetInteger() != v.GetInteger() {
				return false
			}
		}
		if s.Lower.Value == nil && s.Upper.Value == nil {
			return true
		}
		k := row[key[len(s.Equal)]].GetInteger()
		below := s.Lower.Value != nil && (k < s.Lower.Value.GetInteger() || k == s.Lower.Value.GetInteger() && s.Lower.Open)
		above := s.Upper.Value != nil && (k > s.Upper.Value.GetInteger() || k == s.Upper.Value.GetInteger() && s.Upper.Open)
		return !below && !above
	}
	return func(yield func([]*s4qlpb.Value, error) bool) {
		x.f.scans++
		x.f.scanned++
		defer func() { x.f.scans-- }()
		rows := slices.Clone(t.rows)
		slices.SortFunc(rows, func(a, b []*s4qlpb.Value) int {
			for _, position := range key {
				c := cmp.Compare(a[position].GetInteger(), b[position].GetInteger())
				if descending[position] {
					c = -c
				}
				if c != 0 {
					return c
				}
			}
			return 0
		})
		if s.Backward {
			slices.Reverse(rows)
		}
		rows = slices.DeleteFunc(rows, func(row []*s4qlpb.Value) bool { return !within(row) })
		for i, row := range rows {
			if i == x.f.scanPanics {
				panic("the scan panics")
			}
			if i == x.f.scanFails {
				yield(nil, errors.New("the scan fails"))
				return
			}
			x.f.rowsGiven++
			if !yield(row, nil) {
				return
			}
		}
	}
}

// get records its call, as "get", the name, and the integers of the key, with a comma between two,
// and gives the row whose primary key is the key, or nil.
func (x fakeTx) get(_ context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	x.f.calls = append(x.f.calls, fmt.Sprint("get ", name, " ", rowText(key)))
	if x.f.getFails != nil {
		return nil, x.f.getFails
	}
	t := x.f.tables[fold(name)]
	for _, row := range t.rows {
		if slices.EqualFunc(t.ct.GetPrimaryKey(), key, func(position uint32, v *s4qlpb.Value) bool { return row[position].GetInteger() == v.GetInteger() }) {
			x.f.rowsGiven++
			return row, nil
		}
	}
	return nil, nil
}

// createIndex records its call, as "create index", the name, and the table.
func (x fakeTx) createIndex(_ context.Context, ci *s4qlpb.CreateIndex) error {
	x.f.calls = append(x.f.calls, fmt.Sprint("create index ", ci.GetIndex(), " ", ci.GetTable()))
	t := x.f.tables[fold(ci.GetTable())]
	t.indexes = append(t.indexes, ci)
	return nil
}

// dropIndex records its call, as "drop index" and the name.
func (x fakeTx) dropIndex(_ context.Context, name string) error {
	x.f.calls = append(x.f.calls, "drop index "+name)
	for _, t := range x.f.tables {
		t.indexes = slices.DeleteFunc(t.indexes, func(ix *s4qlpb.CreateIndex) bool { return fold(ix.GetIndex()) == fold(name) })
	}
	return nil
}

func (x fakeTx) createTable(_ context.Context, ct *s4qlpb.CreateTable) error {
	x.f.calls = append(x.f.calls, "create "+ct.GetTable())
	x.f.tables[fold(ct.GetTable())] = &fakeTable{ct: ct}
	return nil
}

func (x fakeTx) dropTable(_ context.Context, name string) error {
	x.f.calls = append(x.f.calls, "drop "+name)
	delete(x.f.tables, fold(name))
	return nil
}

func (x fakeTx) upsert(_ context.Context, name string, row []*s4qlpb.Value) error {
	x.f.calls = append(x.f.calls, fmt.Sprint("upsert ", name, " ", rowText(row)))
	t := x.f.tables[fold(name)]
	k := t.ct.GetPrimaryKey()[0]
	t.rows = slices.DeleteFunc(t.rows, func(r []*s4qlpb.Value) bool { return r[k].GetInteger() == row[k].GetInteger() })
	t.rows = append(t.rows, row)
	return nil
}

// insertNew inserts the row if the table has no row with its key. It does not measure the row, and
// fails with insertFails before it looks for the key, as the core fails for a row that is too big.
func (x fakeTx) insertNew(_ context.Context, name string, row []*s4qlpb.Value) (bool, error) {
	if x.f.insertPanics {
		panic("the insert panics")
	}
	x.f.calls = append(x.f.calls, fmt.Sprint("insert ", name, " ", rowText(row)))
	if x.f.insertFails != nil {
		return false, x.f.insertFails
	}
	t := x.f.tables[fold(name)]
	k := t.ct.GetPrimaryKey()[0]
	if slices.ContainsFunc(t.rows, func(r []*s4qlpb.Value) bool { return r[k].GetInteger() == row[k].GetInteger() }) {
		return false, nil
	}
	t.rows = append(t.rows, row)
	return true, nil
}

// delete records its call, as "delete", the name, and the integer key, and deletes the row, or fails
// with deleteFails.
func (x fakeTx) delete(_ context.Context, name string, key []*s4qlpb.Value) (bool, error) {
	x.f.calls = append(x.f.calls, fmt.Sprint("delete ", name, " ", key[0].GetInteger()))
	if x.f.deleteFails != nil {
		return false, x.f.deleteFails
	}
	t := x.f.tables[fold(name)]
	k := t.ct.GetPrimaryKey()[0]
	n := len(t.rows)
	t.rows = slices.DeleteFunc(t.rows, func(r []*s4qlpb.Value) bool { return r[k].GetInteger() == key[0].GetInteger() })
	return len(t.rows) < n, nil
}

// keys records its call, as "keys" and the name, and runs f with a set of integer keys in memory,
// which records each add, as "add" and the key, and each key that it gives, as "key" and the key.
func (x fakeTx) keys(_ context.Context, name string, f func(keySet) error) error {
	x.f.calls = append(x.f.calls, "keys "+name)
	return f(&fakeKeys{f: x.f})
}

type fakeKeys struct {
	f    *fake
	keys []int64
}

func (k *fakeKeys) add(_ context.Context, key []*s4qlpb.Value) error {
	k.f.calls = append(k.f.calls, fmt.Sprint("add ", key[0].GetInteger()))
	if k.f.addFails != nil {
		return k.f.addFails
	}
	if !slices.Contains(k.keys, key[0].GetInteger()) {
		k.keys = append(k.keys, key[0].GetInteger())
	}
	return nil
}

func (k *fakeKeys) all(context.Context) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		if k.f.readKeysFails != nil {
			yield(nil, k.f.readKeysFails)
			return
		}
		var last *int64
		for {
			var next *int64
			for _, key := range k.keys {
				if (last == nil || key > *last) && (next == nil || key < *next) {
					next = &key
				}
			}
			if next == nil {
				return
			}
			k.f.calls = append(k.f.calls, fmt.Sprint("key ", *next))
			if !yield([]*s4qlpb.Value{integer(*next)}, nil) {
				return
			}
			last = next
		}
	}
}

// rowText of the integers and NULLs of the row, with a comma between two.
func rowText(row []*s4qlpb.Value) string {
	var values []string
	for _, v := range row {
		if v.HasNull() {
			values = append(values, "null")
		} else {
			values = append(values, fmt.Sprint(v.GetInteger()))
		}
	}
	return strings.Join(values, ",")
}

func (x fakeTx) position() format.Name {
	return format.EmptyLog
}

func (x fakeTx) annotate(_ context.Context, sql string, args []*s4qlpb.Value) error {
	call := "annotate " + sql
	for _, a := range args {
		call += fmt.Sprint(" with ", a.GetInteger())
	}
	x.f.calls = append(x.f.calls, call)
	return x.f.annotateFails
}

func (x fakeTx) commit(context.Context) error {
	x.f.calls = append(x.f.calls, "commit")
	x.f.open--
	return x.f.commitFails
}

func (x fakeTx) rollback(context.Context) error {
	x.f.calls = append(x.f.calls, "rollback")
	x.f.open--
	return nil
}

// The calls of a transaction that spans statements, which no statement of its own makes.
func (x fakeTx) serving(context.Context) error    { return nil }
func (x fakeTx) savepoint(context.Context) error  { return nil }
func (x fakeTx) release(context.Context) error    { return nil }
func (x fakeTx) rollbackTo(context.Context) error { return nil }
func (x fakeTx) lost() error                      { return nil }

func (x fakeTx) end(context.Context) error {
	x.f.calls = append(x.f.calls, "end")
	x.f.open--
	if x.f.endPanics {
		panic("the end panics")
	}
	return x.f.endFails
}

// collect the rows of src as text, and its error.
func collect(ctx context.Context, src rowSource) ([]string, error) {
	var rows []string
	for row, err := range src.rows(ctx) {
		if err != nil {
			return rows, err
		}
		values := make([]string, len(row))
		for i, v := range row {
			values[i] = fmt.Sprint(v.GetInteger())
		}
		rows = append(rows, strings.Join(values, ","))
	}
	return rows, nil
}

func TestScan(t *testing.T) {
	t.Run("should give the rows in the order of the key, with the columns of the table", func(t *testing.T) {
		f := newFake().with("T1", []string{"K", "w"}, []int64{3, 30}, []int64{-1, 10}, []int64{2, 20})
		s := &scan{r: fakeTx{f}, table: f.tables["t1"].ct}
		rows, err := collect(t.Context(), s)
		is.NotError(t, err)
		is.Equal(t, "-1,10 2,20 3,30", strings.Join(rows, " "))
		is.Equal(t, "[{K INT} {w INT}]", fmt.Sprint(s.columns()))
		is.Equal(t, 0, f.scans)
	})

	t.Run("should end the scan of the storage when the consumer stops early", func(t *testing.T) {
		f := newFake().with("t1", []string{"k"}, []int64{1}, []int64{2})
		s := &scan{r: fakeTx{f}, table: f.tables["t1"].ct}
		for _, err := range s.rows(t.Context()) {
			is.NotError(t, err)
			is.Equal(t, 1, f.scans)
			break
		}
		is.Equal(t, 0, f.scans)
	})

	t.Run("should end with the error of the context when it ends in the middle, and end the scan", func(t *testing.T) {
		f := newFake().with("t1", []string{"k"}, []int64{1}, []int64{2}, []int64{3})
		s := &scan{r: fakeTx{f}, table: f.tables["t1"].ct}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var got []string
		var last error
		for row, err := range s.rows(ctx) {
			if err != nil {
				last = err
				continue
			}
			got = append(got, fmt.Sprint(row[0].GetInteger()))
			cancel()
		}
		is.Equal(t, "[1]", fmt.Sprint(got))
		is.True(t, errors.Is(last, context.Canceled))
		is.Equal(t, 0, f.scans)
	})

	t.Run("should end at once with the error of a context that has ended", func(t *testing.T) {
		f := newFake().with("t1", []string{"k"}, []int64{1}).with("t2", []string{"k"})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for _, table := range []string{"t1", "t2"} {
			rows, err := collect(ctx, &scan{r: fakeTx{f}, table: f.tables[table].ct})
			is.True(t, errors.Is(err, context.Canceled), table)
			is.Equal(t, 0, len(rows))
		}
	})

	t.Run("should end with an error of the storage in the middle", func(t *testing.T) {
		f := newFake().with("t1", []string{"k"}, []int64{1}, []int64{2}, []int64{3})
		f.scanFails = 1
		rows, err := collect(t.Context(), &scan{r: fakeTx{f}, table: f.tables["t1"].ct})
		is.Equal(t, "the scan fails", err.Error())
		is.Equal(t, "[1]", fmt.Sprint(rows))
		is.Equal(t, 0, f.scans)
	})
}

func TestValues(t *testing.T) {
	t.Run("should give its rows", func(t *testing.T) {
		v := &values{data: [][]*s4qlpb.Value{{integer(1)}, {integer(3)}}}
		rows, err := collect(t.Context(), v)
		is.NotError(t, err)
		is.Equal(t, "[1 3]", fmt.Sprint(rows))
	})

	t.Run("should stop early, and end with the error of a context that ended", func(t *testing.T) {
		v := &values{data: [][]*s4qlpb.Value{{integer(1)}, {integer(2)}}}
		n := 0
		for _, err := range v.rows(t.Context()) {
			is.NotError(t, err)
			n++
			break
		}
		is.Equal(t, 1, n)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := collect(ctx, v)
		is.True(t, errors.Is(err, context.Canceled))
	})

	t.Run("should not build the values of a row of another number of values than its columns, an internal error", func(t *testing.T) {
		row := []Expr{&Integer{Value: 1}, &Integer{Value: 2}}
		for _, positions := range [][]int{{0}, {0, 1, 2}} {
			_, _, err := buildValues(&Values{Rows: [][]Expr{row}}, nil, positions, 3)
			is.True(t, errors.Is(err, ErrInternal), fmt.Sprint(positions))
			is.Equal(t, fmt.Sprint("s4ql: internal error: a row of 2 values for ", len(positions), " columns"), err.Error())
		}
	})
}

func TestProject(t *testing.T) {
	t.Run("should give the columns at its positions, and pass on an error and an early stop", func(t *testing.T) {
		f := newFake().with("t1", []string{"a", "b", "c"}, []int64{1, 2, 3}, []int64{4, 5, 6})
		p := &project{src: &scan{r: fakeTx{f}, table: f.tables["t1"].ct}, positions: []int{2, 0}}
		rows, err := collect(t.Context(), p)
		is.NotError(t, err)
		is.Equal(t, "[3,1 6,4]", fmt.Sprint(rows))
		is.Equal(t, "[{c INT} {a INT}]", fmt.Sprint(p.columns()))
		for _, err := range p.rows(t.Context()) {
			is.NotError(t, err)
			break
		}
		is.Equal(t, 0, f.scans)
		f.scanFails = 1
		rows, err = collect(t.Context(), p)
		is.Equal(t, "the scan fails", err.Error())
		is.Equal(t, "[3,1]", fmt.Sprint(rows))
	})
}

func TestFilter(t *testing.T) {
	// query src with args on the fake f, forward, and give its rows, the calls of the fake, and the
	// access of its scan.
	query := func(t *testing.T, f *fake, src string, scanAll bool, args ...*s4qlpb.Value) (string, string, access) {
		t.Helper()
		s, err := stmt.Check(src)
		is.NotError(t, err, src)
		f.calls, f.scanned = nil, 0
		q, err := buildQuery(t.Context(), fakeTx{f}, NewPlan(s).(*Query), args, 1_000_000_000, readingOf(scanAll))
		is.NotError(t, err, src)
		rows, err := collect(t.Context(), q)
		is.NotError(t, err, src)
		return fmt.Sprint(rows), fmt.Sprint(f.calls), scanOf(q).access
	}
	newTable := func() *fake {
		return newFake().with("t", []string{"w", "k"}, []int64{40, 4}, []int64{10, 1}, []int64{30, 3}, []int64{20, 2})
	}

	// reads checks the calls of the fake f, calls, for the statement src, which read by the access a
	// that the rules chose: no row and the row of a whole primary key, which do not depend on the
	// indexes of the table, look none up and scan nothing, and every other access looks them up once,
	// before its one scan.
	reads := func(t *testing.T, f *fake, src, calls string, a access) {
		t.Helper()
		want := 1
		if a.none || a.point != nil {
			want = 0
		}
		is.Equal(t, want, strings.Count(calls, "indexes "), "the look-ups of the indexes of "+src)
		is.Equal(t, want, f.scanned, "the scans of "+src)
		is.Equal(t, map[bool]int{true: 1}[a.point != nil], strings.Count(calls, " get "), "the gets of "+src)
	}

	t.Run("should read the row of the key, the rows within the tightest bounds of the key, or no row, where the conditions allow it, look the indexes up only for another access, and give the rows of a scan of every row", func(t *testing.T) {
		cases := []struct {
			src   string
			args  []*s4qlpb.Value
			rows  string
			calls string
		}{
			// The row of the key, by a get, without the indexes.
			{src: "select * from t where k = 3", rows: "[30,3]", calls: "[table t get t 3]"},
			{src: "select k from t where K = ?", args: []*s4qlpb.Value{integer(2)}, rows: "[2]", calls: "[table t get t 2]"},
			{src: "select * from t where k = 9", rows: "[]", calls: "[table t get t 9]"},
			{src: "select * from t where k = 2 and k > 2", rows: "[]", calls: "[table t get t 2]"},
			{src: "select * from t where k > 3 and k = 2 and k = 4", rows: "[]", calls: "[table t get t 2]"},
			{src: "select * from t where w = 20 and k = 2", rows: "[20,2]", calls: "[table t get t 2]"},
			// The rows within bounds of the key, which the primary key wins for a table without indexes.
			{src: "select * from t where k > 1", rows: "[20,2 30,3 40,4]", calls: "[table t indexes t range t > 1]"},
			{src: "select * from t where k >= 2 and k < 4", rows: "[20,2 30,3]", calls: "[table t indexes t range t >= 2 < 4]"},
			{src: "select * from t where k <= 3 and k < 3", rows: "[10,1 20,2]", calls: "[table t indexes t range t < 3]"},
			{src: "select * from t where k < 3 and k <= 3", rows: "[10,1 20,2]", calls: "[table t indexes t range t < 3]"},
			{src: "select * from t where k <= 2 and k <= 3", rows: "[10,1 20,2]", calls: "[table t indexes t range t <= 2]"},
			{src: "select * from t where k <= 3 and k <= 2", rows: "[10,1 20,2]", calls: "[table t indexes t range t <= 2]"},
			{src: "select * from t where k > 1 and k >= 2 and k > 2", rows: "[30,3 40,4]", calls: "[table t indexes t range t > 2]"},
			{src: "select * from t where k >= 2 and k > 1", rows: "[20,2 30,3 40,4]", calls: "[table t indexes t range t >= 2]"},
			{src: "select * from t where k > ? and k < ?", args: []*s4qlpb.Value{integer(3), integer(2)}, rows: "[]", calls: "[table t indexes t range t > 3 < 2]"},
			{src: "select * from t where w > 10 and k < 4 and w != 30", rows: "[20,2]", calls: "[table t indexes t range t < 4]"},
			{src: "select * from t where k is not null and k >= 4", rows: "[40,4]", calls: "[table t indexes t range t >= 4]"},
			// No use of the key: every row, by a scan forward, which the fake does not record.
			{src: "select * from t where k != 2", rows: "[10,1 30,3 40,4]", calls: "[table t indexes t]"},
			{src: "select * from t where w = 20", rows: "[20,2]", calls: "[table t indexes t]"},
			{src: "select * from t where w >= 20 and w < 40", rows: "[20,2 30,3]", calls: "[table t indexes t]"},
			{src: "select * from t where k is not null", rows: "[10,1 20,2 30,3 40,4]", calls: "[table t indexes t]"},
			// A test for NULL of a column that is not of the key and takes no NULL is left to the filter.
			{src: "select * from t where w is null", rows: "[]", calls: "[table t indexes t]"},
			// No row, without the indexes: the key is never NULL, and a comparison with NULL is never
			// true.
			{src: "select * from t where k is null", rows: "[]", calls: "[table t]"},
			{src: "select * from t where w = null and k = 1", rows: "[]", calls: "[table t]"},
			{src: "select * from t where k > 1 and w < ?", args: []*s4qlpb.Value{null()}, rows: "[]", calls: "[table t]"},
		}
		for _, c := range cases {
			f := newTable()
			f.tables["t"].ct.SetPrimaryKey([]uint32{1})
			rows, calls, a := query(t, f, c.src, false, c.args...)
			is.Equal(t, c.rows, rows, c.src)
			is.Equal(t, c.calls, calls, c.src)
			reads(t, f, c.src, calls, a)
			// A scan of every row that the test forces chooses nothing, so it looks no index up.
			rows, calls, _ = query(t, f, c.src, true, c.args...)
			is.Equal(t, c.rows, rows, "every row: "+c.src)
			is.Equal(t, "[table t]", calls, "every row: "+c.src)
			is.Equal(t, 1, f.scanned, "every row: "+c.src)
		}
	})

	t.Run("should read by a primary key of two columns as by one: the row of the whole key, the rows of its first column pinned, with bounds on the second, or within bounds of the first, no row, and every row for a condition on the second column alone", func(t *testing.T) {
		for _, c := range []struct{ src, rows, calls string }{
			// The whole key, in the order of the key whatever the order of the text, by a get.
			{src: "select * from t where k = 3 and w = 30", rows: "[30,3]", calls: "[table t get t 3,30]"},
			{src: "select * from t where w = 30 and k = 3", rows: "[30,3]", calls: "[table t get t 3,30]"},
			{src: "select * from t where k = 3 and w = 31", rows: "[]", calls: "[table t get t 3,31]"},
			// The first column pinned, without and with bounds on the second.
			{src: "select * from t where k = 3", rows: "[30,3]", calls: "[table t indexes t range t = 3]"},
			{src: "select * from t where k = 3 and w > 10 and w <= 30", rows: "[30,3]", calls: "[table t indexes t range t = 3 > 10 <= 30]"},
			{src: "select * from t where w < 30 and k = 3", rows: "[]", calls: "[table t indexes t range t = 3 < 30]"},
			// Bounds on the first column, which a condition on the second does not tighten.
			{src: "select * from t where k > 2 and w < 40", rows: "[30,3]", calls: "[table t indexes t range t > 2]"},
			// The second column alone neither pins nor bounds the key: every row.
			{src: "select * from t where w = 30", rows: "[30,3]", calls: "[table t indexes t]"},
			{src: "select * from t where w >= 30", rows: "[30,3 40,4]", calls: "[table t indexes t]"},
			// No row: neither column of the key is ever NULL.
			{src: "select * from t where k is null", rows: "[]", calls: "[table t]"},
			{src: "select * from t where k = 3 and w is null", rows: "[]", calls: "[table t]"},
		} {
			f := newTable()
			f.tables["t"].ct.SetPrimaryKey([]uint32{1, 0})
			rows, calls, a := query(t, f, c.src, false)
			is.Equal(t, c.rows, rows, c.src)
			is.Equal(t, c.calls, calls, c.src)
			reads(t, f, c.src, calls, a)
		}
	})

	t.Run("should end with the error of a context that has ended, for every way of reading the rows", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for _, src := range []string{"select * from t where k = 1", "select * from t where k > 1", "select * from t where k is null", "select * from t where w = 1"} {
			f := newTable()
			f.tables["t"].ct.SetPrimaryKey([]uint32{1})
			s, err := stmt.Check(src)
			is.NotError(t, err)
			q, err := buildQuery(t.Context(), fakeTx{f}, NewPlan(s).(*Query), nil, 1_000_000_000, reading{})
			is.NotError(t, err)
			_, err = collect(ctx, q)
			is.True(t, errors.Is(err, context.Canceled), src)
		}
	})

	t.Run("should read as the rules choose where the path names an index that the table does not have", func(t *testing.T) {
		f := newTable().index("i", "t", 1)
		s, err := stmt.Check("select * from t where k = 2")
		is.NotError(t, err)
		q, err := buildQuery(t.Context(), fakeTx{f}, NewPlan(s).(*Query), nil, 1_000_000_000, reading{path: path{kind: pathIndex, index: "nosuch"}})
		is.NotError(t, err)
		rows, err := collect(t.Context(), q)
		is.NotError(t, err)
		is.Equal(t, "[20,2]", fmt.Sprint(rows))
		is.Equal(t, "i = 2", accessText(scanOf(q).access))
	})

	t.Run("should pass on an error of the read of the indexes of the table of a select, an update, and a delete that the indexes decide, and scan nothing", func(t *testing.T) {
		for _, src := range []string{"select * from t where w > 0", "update t set w = 1 where w > 0", "delete from t where w > 0"} {
			f := newTable()
			f.indexesFails = errors.New("the indexes fail")
			s, err := stmt.Check(src)
			is.NotError(t, err)
			switch p := NewPlan(s).(type) {
			case *Query:
				_, err = buildQuery(t.Context(), fakeTx{f}, p, nil, 1_000_000_000, reading{})
			default:
				_, err = buildSink(t.Context(), fakeTx{f}, p, nil, bugs{}, 1_000_000_000, reading{})
			}
			is.True(t, errors.Is(err, f.indexesFails), src)
			is.Equal(t, "[table t indexes t]", fmt.Sprint(f.calls), src)
			is.Equal(t, 0, f.scanned, src)
		}
	})

	t.Run("should pass on an error of the scan and of the get, and stop the scan at an early stop", func(t *testing.T) {
		f := newTable()
		f.tables["t"].ct.SetPrimaryKey([]uint32{1})
		f.scanFails = 2
		for _, src := range []string{"select * from t where k > 0", "select * from t where w > 0"} {
			s, err := stmt.Check(src)
			is.NotError(t, err)
			q, err := buildQuery(t.Context(), fakeTx{f}, NewPlan(s).(*Query), nil, 1_000_000_000, reading{})
			is.NotError(t, err)
			rows, err := collect(t.Context(), q)
			is.Equal(t, "the scan fails", fmt.Sprint(err), src)
			is.Equal(t, "[10,1 20,2]", fmt.Sprint(rows), src)
			for _, err := range q.rows(t.Context()) {
				is.NotError(t, err)
				break
			}
			is.Equal(t, 0, f.scans, src)
		}
		f.getFails = errors.New("the get fails")
		s, err := stmt.Check("select * from t where k = 1")
		is.NotError(t, err)
		q, err := buildQuery(t.Context(), fakeTx{f}, NewPlan(s).(*Query), nil, 1_000_000_000, reading{})
		is.NotError(t, err)
		rows, err := collect(t.Context(), q)
		is.Equal(t, "the get fails", fmt.Sprint(err))
		is.Equal(t, "[]", fmt.Sprint(rows))
	})

	t.Run("should fail with an internal error for filters that no supported statement gives", func(t *testing.T) {
		scanT := &Scan{Table: stmt.Name{Value: "t"}}
		k := stmt.Name{Value: "k"}
		cases := []struct {
			name string
			root Node
			err  string
		}{
			{name: "a filter of a filter", root: &Filter{Source: &Filter{Source: scanT}}, err: "a filter of a node of type *engine.filter"},
			{name: "a filter of VALUES", root: &Filter{Source: &Values{}}, err: "a query of a node of type *engine.Values"},
			{name: "a condition without an operator", root: &Filter{Source: scanT, Conditions: []Condition{{Column: k}}}, err: "a condition with the operator 0"},
			{name: "a test for NULL with a value", root: &Filter{Source: scanT, Conditions: []Condition{{Column: k, Operator: stmt.OpIsNull, Value: &Integer{Value: 1}}}}, err: "a test for NULL with a value"},
			{name: "a comparison without a value", root: &Filter{Source: scanT, Conditions: []Condition{{Column: k, Operator: stmt.OpLess}}}, err: "a comparison without a value"},
			{name: "a comparison of a parameter without its argument", root: &Filter{Source: scanT, Conditions: []Condition{{Column: k, Operator: stmt.OpLess, Value: &Parameter{Number: 1}}}}, err: "the parameter 1 of a statement with 0 arguments"},
		}
		for _, c := range cases {
			_, err := buildQuery(t.Context(), fakeTx{newTable()}, &Query{Root: c.root}, nil, 1_000_000_000, reading{})
			is.True(t, errors.Is(err, ErrInternal), c.name)
			is.Equal(t, "s4ql: internal error: "+c.err, fmt.Sprint(err), c.name)
		}
		text := s4qlpb.Value_builder{Text: new("a")}.Build()
		// A comparison that fails gives 0, the zero value, with its error.
		n, err := compare(integer(1), text)
		is.Equal(t, "s4ql: internal error: a comparison of a value of kind integer with one of kind text", fmt.Sprint(err))
		is.Equal(t, 0, n)
		n, err = compare(&s4qlpb.Value{}, &s4qlpb.Value{})
		is.Equal(t, "s4ql: internal error: a comparison of values of kind not set", fmt.Sprint(err))
		is.Equal(t, 0, n)
		// A condition that cannot be tested on the row is not true of it, and fails.
		for _, c := range []struct {
			c   condition
			err string
		}{
			{c: condition{position: 2, op: stmt.OpEqual, value: integer(1)}, err: "a condition of the column at 2 of a row of 1 values"},
			{c: condition{position: -1, op: stmt.OpIsNull}, err: "a condition of the column at -1 of a row of 1 values"},
			{c: condition{position: 1, op: stmt.OpIsNull}, err: "a condition of the column at 1 of a row of 1 values"},
			{c: condition{position: 0, op: stmt.OpEqual, value: text}, err: "a comparison of a value of kind integer with one of kind text"},
			{c: condition{position: 0, op: stmt.Operator(99), value: integer(1)}, err: "a condition with the operator 99"},
		} {
			ok, err := c.c.holds([]*s4qlpb.Value{integer(1)})
			is.True(t, !ok, c.err)
			is.True(t, errors.Is(err, ErrInternal), c.err)
			is.Equal(t, "s4ql: internal error: "+c.err, fmt.Sprint(err))
		}
		_, err = chooseAccess(newTable().tables["t"].ct, nil, []condition{{position: 0, op: stmt.OpLess, value: integer(1)}, {position: 0, op: stmt.OpLess, value: text}}, bugs{})
		is.True(t, errors.Is(err, ErrInternal))
	})

	t.Run("should end with the internal error of a condition that cannot be tested on a row", func(t *testing.T) {
		f := newTable()
		text := s4qlpb.Value_builder{Text: new("a")}.Build()
		// The first condition leaves out the first row, so the second meets only the rows after it.
		q := &filter{src: &scan{r: fakeTx{f}, table: f.tables["t"].ct}, conditions: []condition{
			{position: 0, op: stmt.OpGreater, value: integer(10)},
			{position: 1, op: stmt.OpEqual, value: text},
		}}
		rows, err := collect(t.Context(), q)
		is.True(t, errors.Is(err, ErrInternal), fmt.Sprint(err))
		is.Equal(t, "s4ql: internal error: a comparison of a value of kind integer with one of kind text", fmt.Sprint(err))
		is.Equal(t, "[]", fmt.Sprint(rows))
		n := 0
		for row, err := range q.rows(t.Context()) {
			n++
			is.True(t, row == nil)
			is.True(t, errors.Is(err, ErrInternal), fmt.Sprint(err))
		}
		is.Equal(t, 1, n)
	})
}

func TestInsert(t *testing.T) {
	ints := func(vs ...int64) *values {
		v := &values{}
		for _, x := range vs {
			v.data = append(v.data, []*s4qlpb.Value{integer(x)})
		}
		return v
	}

	t.Run("should insert each row only if its key is new, with one call of the storage for each, and count the rows", func(t *testing.T) {
		f := newFake().with("T1", []string{"v"})
		i := &insert{w: fakeTx{f}, table: f.tables["t1"].ct, src: ints(2, 1)}
		n, err := i.run(t.Context())
		is.NotError(t, err)
		is.Equal(t, int64(2), n)
		is.Equal(t, "[insert T1 2 insert T1 1]", fmt.Sprint(f.calls))
	})

	t.Run("should fail with SQLite's error for a key that the table has, also from an earlier row, with the stored names", func(t *testing.T) {
		f := newFake().with("Tb", []string{"Vb"}, []int64{1})
		i := &insert{w: fakeTx{f}, table: f.tables["tb"].ct, src: ints(1)}
		_, err := i.run(t.Context())
		var se *sqlerr.Error
		is.True(t, errors.As(err, &se))
		is.Equal(t, sqlerr.Error{Code: 19, ExtendedCode: 1555, Message: "UNIQUE constraint failed: Tb.Vb", Offset: -1}, *se)

		f = newFake().with("t", []string{"v"})
		i = &insert{w: fakeTx{f}, table: f.tables["t"].ct, src: ints(5, 5)}
		n, err := i.run(t.Context())
		is.Equal(t, "UNIQUE constraint failed: t.v", err.Error())
		is.Equal(t, int64(1), n)
	})

	t.Run("should pass on an error of the storage, and of the source", func(t *testing.T) {
		f := newFake().with("t", []string{"v"})
		f.insertFails = errors.New("insert fails")
		n, err := (&insert{w: fakeTx{f}, table: f.tables["t"].ct, src: ints(1)}).run(t.Context())
		is.Equal(t, "insert fails", err.Error())
		is.Equal(t, int64(0), n)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		f.insertFails = nil
		_, err = (&insert{w: fakeTx{f}, table: f.tables["t"].ct, src: ints(1)}).run(ctx)
		is.True(t, errors.Is(err, context.Canceled))
		is.Equal(t, "[insert t 1]", fmt.Sprint(f.calls))
	})

	t.Run("should check the type of each value of every row, and fail with an internal error for rows that its expressions do not match", func(t *testing.T) {
		f := newFake().with("t", []string{"v"})
		ct := f.tables["t"].ct
		one := []Expr{&Integer{Value: 1}}
		is.NotError(t, (&insert{w: fakeTx{f}, table: ct, src: ints(1, 2), exprs: [][]Expr{one, one}}).check(t.Context()))

		src := ints(1)
		src.data = append(src.data, []*s4qlpb.Value{s4qlpb.Value_builder{Text: new("a")}.Build()})
		err := (&insert{w: fakeTx{f}, table: ct, src: src, exprs: [][]Expr{one, {&Text{Value: "a"}}}}).check(t.Context())
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		is.Equal(t, "s4ql: not supported: a string literal for the column t.v of type INT", err.Error())

		for _, c := range []struct {
			i    *insert
			want string
		}{
			{i: &insert{table: ct, src: ints(1, 2), exprs: [][]Expr{one}}, want: "an insert of 2 rows from 1 rows of expressions"},
			{i: &insert{table: ct, src: ints(1), exprs: [][]Expr{one, one}}, want: "an insert of 1 rows from 2 rows of expressions"},
			{i: &insert{table: ct, src: ints(1), exprs: [][]Expr{{}}}, want: "an insert of 1 values from 0 expressions into 1 columns"},
			{i: &insert{table: ct, src: ints(1), exprs: [][]Expr{{one[0], one[0]}}}, want: "an insert of 1 values from 2 expressions into 1 columns"},
			{i: &insert{table: ct, src: &values{data: [][]*s4qlpb.Value{{integer(1), integer(1)}}}, exprs: [][]Expr{{one[0], one[0]}}}, want: "an insert of 2 values from 2 expressions into 1 columns"},
			{i: &insert{table: ct, src: &values{data: [][]*s4qlpb.Value{{integer(1), integer(1)}}}, exprs: [][]Expr{one}}, want: "an insert of 2 values from 1 expressions into 1 columns"},
		} {
			err := c.i.check(t.Context())
			is.True(t, errors.Is(err, ErrInternal), c.want)
			is.Equal(t, "s4ql: internal error: "+c.want, err.Error())
		}
	})

	t.Run("should ask the storage to insert a row whose key the table has, so that it measures the row, and pass on its error", func(t *testing.T) {
		f := newFake().with("t", []string{"v"}, []int64{1})
		_, err := (&insert{w: fakeTx{f}, table: f.tables["t"].ct, src: ints(1)}).run(t.Context())
		is.Equal(t, "UNIQUE constraint failed: t.v", err.Error())
		is.Equal(t, "[insert t 1]", fmt.Sprint(f.calls))
		f.insertFails = errors.New("string or blob too big")
		_, err = (&insert{w: fakeTx{f}, table: f.tables["t"].ct, src: ints(1)}).run(t.Context())
		is.Equal(t, "string or blob too big", err.Error())
	})

	t.Run("should write without the check with the planted bug, so that the row replaces the row with its key", func(t *testing.T) {
		f := newFake().with("t", []string{"v", "w"}, []int64{1, 1})
		row := &values{data: [][]*s4qlpb.Value{{integer(1), integer(2)}}}
		n, err := (&insert{w: fakeTx{f}, table: f.tables["t"].ct, src: row, skipUnique: true}).run(t.Context())
		is.NotError(t, err)
		is.Equal(t, int64(1), n)
		is.Equal(t, "[upsert t 1,2]", fmt.Sprint(f.calls))
		is.Equal(t, 1, len(f.tables["t"].rows))
		is.Equal(t, "1,2", rowText(f.tables["t"].rows[0]))
	})

	t.Run("should take NULL in a column that takes it, of any type, and give SQLite's error for NULL in any other column and in the key, in the order of the table", func(t *testing.T) {
		null, text := null(), s4qlpb.Value_builder{Text: new("a")}.Build()
		ct := s4qlpb.CreateTable_builder{Table: new("t"), PrimaryKey: []uint32{1}, Columns: []*s4qlpb.Column{
			s4qlpb.Column_builder{Name: new("a"), Type: new(s4qlpb.Type_TYPE_INT), Nullable: new(true)}.Build(),
			s4qlpb.Column_builder{Name: new("k"), Type: new(s4qlpb.Type_TYPE_INT)}.Build(),
			s4qlpb.Column_builder{Name: new("b"), Type: new(s4qlpb.Type_TYPE_TEXT)}.Build(),
			s4qlpb.Column_builder{Name: new("c"), Type: new(s4qlpb.Type_TYPE_BLOB), Nullable: new(true)}.Build(),
		}}.Build()
		// The literals of the values of a row.
		literals := func(row []*s4qlpb.Value) []Expr {
			var es []Expr
			for _, v := range row {
				switch {
				case v.HasNull():
					es = append(es, &Null{})
				case v.HasText():
					es = append(es, &Text{Value: v.GetText()})
				default:
					es = append(es, &Integer{Value: v.GetInteger()})
				}
			}
			return es
		}
		for _, c := range []struct {
			row  []*s4qlpb.Value
			want string
		}{
			{row: []*s4qlpb.Value{null, integer(1), text, null}, want: ""},
			{row: []*s4qlpb.Value{null, null, text, null}, want: "NOT NULL constraint failed: t.k"},
			{row: []*s4qlpb.Value{null, integer(1), null, null}, want: "NOT NULL constraint failed: t.b"},
			{row: []*s4qlpb.Value{null, null, null, null}, want: "NOT NULL constraint failed: t.k"},
			// NOT NULL comes before the type of a value, also of a column before it.
			{row: []*s4qlpb.Value{text, integer(1), null, null}, want: "NOT NULL constraint failed: t.b"},
			{row: []*s4qlpb.Value{text, integer(1), text, null}, want: "s4ql: not supported: a string literal for the column t.a of type INT"},
			{row: []*s4qlpb.Value{null, integer(1), text, text}, want: "s4ql: not supported: a string literal for the column t.c of type BLOB"},
		} {
			err := (&insert{table: ct, key: 1, src: &values{data: [][]*s4qlpb.Value{c.row}}, exprs: [][]Expr{literals(c.row)}}).check(t.Context())
			if c.want == "" {
				is.NotError(t, err)
				continue
			}
			is.Equal(t, c.want, err.Error())
			if se, ok := errors.AsType[*sqlerr.Error](err); ok {
				is.Equal(t, sqlerr.Error{Code: 19, ExtendedCode: 1299, Message: c.want, Offset: -1}, *se)
			}
		}
		err := (&insert{table: ct, key: 1, src: &values{data: [][]*s4qlpb.Value{{null, integer(1), nil, null}}}, exprs: [][]Expr{make([]Expr, 4)}}).check(t.Context())
		is.True(t, errors.Is(err, ErrInternal))
		is.Equal(t, "s4ql: internal error: an insert without a value for the column b", err.Error())
	})
}

func TestChangeRows(t *testing.T) {
	// change runs src on the fake f, by the key unless scanAll, and gives its error, the number of
	// rows that it changed, and the calls of the fake.
	change := func(t *testing.T, f *fake, src string, scanAll bool) (string, int64, string) {
		t.Helper()
		s, err := stmt.Check(src)
		is.NotError(t, err, src)
		f.calls = nil
		k, err := buildSink(t.Context(), fakeTx{f}, NewPlan(s), nil, bugs{}, 1_000_000_000, readingOf(scanAll))
		is.NotError(t, err, src)
		if err := k.check(t.Context()); err != nil {
			return err.Error(), 0, fmt.Sprint(f.calls)
		}
		n, err := k.run(t.Context())
		if err != nil {
			return err.Error(), n, fmt.Sprint(f.calls)
		}
		return "", n, fmt.Sprint(f.calls)
	}
	newTable := func() *fake {
		f := newFake().with("t", []string{"w", "k"}, []int64{40, 4}, []int64{10, 1}, []int64{30, 3}, []int64{20, 2})
		f.tables["t"].ct.SetPrimaryKey([]uint32{1})
		return f
	}
	// byRules gives the calls of a statement on w, by the rules unless scanAll: the rules then look
	// the indexes of the table up once, since a condition on w pins no column of the primary key.
	byRules := func(calls string, scanAll bool) string {
		if scanAll {
			return calls
		}
		return strings.Replace(calls, "[table t", "[table t indexes t", 1)
	}

	cases := []struct {
		src string
		err string
		n   int64
		// byKey and all are the calls of the fake by the key and with a scan of every row, which the
		// fake does not record.
		byKey, all string
		// indexes is the number of look-ups of the indexes of the table by the key: one for a WHERE
		// that reads neither the row of the key nor no row, since only then the access depends on
		// them, and none without WHERE.
		indexes int
	}{
		{src: "update t set w = 25 where k = 2", n: 1,
			byKey: "[table t keys t get t 2 add 2 key 2 get t 2 upsert t 25,2]",
			all:   "[table t keys t add 2 key 2 get t 2 upsert t 25,2]"},
		{src: "update t set w = 0", n: 4,
			byKey: "[table t keys t add 1 add 2 add 3 add 4 key 1 get t 1 upsert t 0,1 key 2 get t 2 upsert t 0,2 key 3 get t 3 upsert t 0,3 key 4 get t 4 upsert t 0,4]",
			all:   "[table t keys t add 1 add 2 add 3 add 4 key 1 get t 1 upsert t 0,1 key 2 get t 2 upsert t 0,2 key 3 get t 3 upsert t 0,3 key 4 get t 4 upsert t 0,4]"},
		{src: "update t set k = 5, w = 50 where k > 3", n: 1, indexes: 1,
			byKey: "[table t indexes t keys t range t > 3 add 4 key 4 get t 4 delete t 4 insert t 50,5]",
			all:   "[table t keys t add 4 key 4 get t 4 delete t 4 insert t 50,5]"},
		{src: "update t set k = 9 where k >= 3", err: "UNIQUE constraint failed: t.k", n: 1, indexes: 1,
			byKey: "[table t indexes t keys t range t >= 3 add 3 add 4 key 3 get t 3 delete t 3 insert t 30,9 key 4 get t 4 delete t 4 insert t 40,9]",
			all:   "[table t keys t add 3 add 4 key 3 get t 3 delete t 3 insert t 30,9 key 4 get t 4 delete t 4 insert t 40,9]"},
		{src: "update t set k = 2 where k = 2", n: 1,
			byKey: "[table t keys t get t 2 add 2 key 2 get t 2 upsert t 20,2]",
			all:   "[table t keys t add 2 key 2 get t 2 upsert t 20,2]"},
		{src: "update t set w = null where k = 9", n: 0,
			byKey: "[table t get t 9 keys t get t 9]",
			all:   "[table t keys t]"},
		// The check reads the source once, also for two columns that do not take NULL and get it.
		{src: "update t set k = null, w = null where k = 9", n: 0,
			byKey: "[table t get t 9 keys t get t 9]",
			all:   "[table t keys t]"},
		{src: "update t set w = null where k = 2", err: "NOT NULL constraint failed: t.w",
			byKey: "[table t get t 2]",
			all:   "[table t]"},
		{src: "update t set w = null where k > 2", err: "NOT NULL constraint failed: t.w", indexes: 1,
			byKey: "[table t indexes t range t > 2]",
			all:   "[table t]"},
		{src: "update t set w = 1 where k is null", n: 0, byKey: "[table t keys t]", all: "[table t keys t]"},
		{src: "delete from t where k < 3", n: 2, indexes: 1,
			byKey: "[table t indexes t keys t range t < 3 add 1 add 2 key 1 delete t 1 key 2 delete t 2]",
			all:   "[table t keys t add 1 add 2 key 1 delete t 1 key 2 delete t 2]"},
		{src: "delete from t where w = 30", n: 1, indexes: 1,
			byKey: "[table t indexes t keys t add 3 key 3 delete t 3]",
			all:   "[table t keys t add 3 key 3 delete t 3]"},
		{src: "delete from t where k = 3", n: 1,
			byKey: "[table t keys t get t 3 add 3 key 3 delete t 3]",
			all:   "[table t keys t add 3 key 3 delete t 3]"},
		{src: "delete from t where w = null", n: 0, byKey: "[table t keys t]", all: "[table t keys t]"},
		{src: "delete from t", n: 4,
			byKey: "[table t keys t add 1 add 2 add 3 add 4 key 1 delete t 1 key 2 delete t 2 key 3 delete t 3 key 4 delete t 4]",
			all:   "[table t keys t add 1 add 2 add 3 add 4 key 1 delete t 1 key 2 delete t 2 key 3 delete t 3 key 4 delete t 4]"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			for scanAll, want := range map[bool]string{false: c.byKey, true: c.all} {
				err, n, calls := change(t, newTable(), c.src, scanAll)
				is.Equal(t, c.err, err)
				is.Equal(t, c.n, n)
				is.Equal(t, want, calls, fmt.Sprint("scan all: ", scanAll))
				// A scan of every row that the test forces chooses nothing, so it looks no index up.
				is.Equal(t, map[bool]int{false: c.indexes}[scanAll], strings.Count(calls, "indexes "), fmt.Sprint("scan all: ", scanAll))
			}
		})
	}

	t.Run("should give the error of the read of the check of NOT NULL, and change nothing", func(t *testing.T) {
		for _, scanAll := range []bool{false, true} {
			f := newTable()
			f.scanFails = 0
			err, n, calls := change(t, f, "update t set w = null where w = 20", scanAll)
			is.Equal(t, "the scan fails", err)
			is.Equal(t, int64(0), n)
			is.Equal(t, byRules("[table t]", scanAll), calls, fmt.Sprint("scan all: ", scanAll))
		}
	})

	t.Run("should give the error of the read of the row of a key that the update found", func(t *testing.T) {
		for _, scanAll := range []bool{false, true} {
			f := newTable()
			f.getFails = errors.New("the get fails")
			err, n, calls := change(t, f, "update t set w = 5 where w = 20", scanAll)
			is.Equal(t, "the get fails", err)
			is.Equal(t, int64(0), n)
			is.Equal(t, byRules("[table t keys t add 2 key 2 get t 2]", scanAll), calls, fmt.Sprint("scan all: ", scanAll))
		}
	})

	t.Run("should give the error of the delete of a row, of a delete and of a change of the key", func(t *testing.T) {
		for _, scanAll := range []bool{false, true} {
			for src, want := range map[string]string{
				"delete from t where w = 20":      "[table t keys t add 2 key 2 delete t 2]",
				"update t set k = 5 where w = 20": "[table t keys t add 2 key 2 get t 2 delete t 2]",
			} {
				f := newTable()
				f.deleteFails = errors.New("the delete fails")
				err, n, calls := change(t, f, src, scanAll)
				is.Equal(t, "the delete fails", err, src)
				is.Equal(t, int64(0), n, src)
				is.Equal(t, byRules(want, scanAll), calls, fmt.Sprint(src, ", scan all: ", scanAll))
			}
		}
	})

	t.Run("should give the error of the set of keys, of an add and of a read, and change nothing", func(t *testing.T) {
		for _, scanAll := range []bool{false, true} {
			f := newTable()
			f.addFails = errors.New("the add fails")
			err, n, calls := change(t, f, "delete from t where w >= 20", scanAll)
			is.Equal(t, "the add fails", err)
			is.Equal(t, int64(0), n)
			is.Equal(t, byRules("[table t keys t add 2]", scanAll), calls, fmt.Sprint("scan all: ", scanAll))

			f = newTable()
			f.readKeysFails = errors.New("the read of the keys fails")
			err, n, calls = change(t, f, "delete from t where w >= 20", scanAll)
			is.Equal(t, "the read of the keys fails", err)
			is.Equal(t, int64(0), n)
			is.Equal(t, byRules("[table t keys t add 2 add 3 add 4]", scanAll), calls, fmt.Sprint("scan all: ", scanAll))
		}
	})

	t.Run("should stop at the next key once the context ends, before it changes the row of the key", func(t *testing.T) {
		f := newTable()
		ctx, cancel := context.WithCancel(t.Context())
		src := &scan{r: fakeTx{f}, table: f.tables["t"].ct}
		n, err := changeRows(ctx, fakeTx{f}, "t", 1, src, bugs{}, func(key []*s4qlpb.Value) error {
			f.calls = append(f.calls, fmt.Sprint("change ", key[0].GetInteger()))
			cancel()
			return nil
		})
		is.True(t, errors.Is(err, context.Canceled), fmt.Sprint(err))
		is.Equal(t, int64(1), n)
		is.Equal(t, "[keys t add 1 add 2 add 3 add 4 key 1 change 1 key 2]", fmt.Sprint(f.calls))
	})

	t.Run("should change nothing and count no row of an update and a delete with the planted bug changeNothing, and keep the row of the old key with keepOldKey", func(t *testing.T) {
		for _, c := range []struct {
			src   string
			bugs  bugs
			n     int64
			calls string
		}{
			{src: "update t set w = 5 where k = 2", bugs: bugs{changeNothing: true}, calls: "[table t]"},
			{src: "delete from t", bugs: bugs{changeNothing: true}, calls: "[table t]"},
			{src: "update t set k = 5 where k = 2", bugs: bugs{keepOldKey: true}, n: 1, calls: "[table t keys t get t 2 add 2 key 2 get t 2 insert t 20,5]"},
		} {
			f := newTable()
			s, err := stmt.Check(c.src)
			is.NotError(t, err)
			k, err := buildSink(t.Context(), fakeTx{f}, NewPlan(s), nil, c.bugs, 1_000_000_000, reading{})
			is.NotError(t, err)
			is.NotError(t, k.check(t.Context()))
			n, err := k.run(t.Context())
			is.NotError(t, err)
			is.Equal(t, c.n, n, c.src)
			is.Equal(t, c.calls, fmt.Sprint(f.calls), c.src)
		}
	})

	t.Run("should give an internal error for a plan of a change that no statement gives", func(t *testing.T) {
		name := stmt.Name{Raw: "t", Value: "t"}
		for _, c := range []struct {
			plan Plan
			key  []uint32
			want string
		}{
			{plan: &Delete{Table: name, Source: &Values{}}, key: []uint32{1}, want: "a change of the rows of a node of type *engine.Values"},
			{plan: &Delete{Table: name, Source: &Filter{Source: &Values{}}}, key: []uint32{1}, want: "a change of the rows of a node of type *engine.Values"},
			{plan: &Delete{Table: name, Source: &Scan{Table: name}}, key: []uint32{0, 1}, want: "a change of a table of 2 columns with the key [0 1]"},
			{plan: &Delete{Table: name, Source: &Scan{Table: name}}, key: []uint32{2}, want: "a change of a table of 2 columns with the key [2]"},
			{plan: &Update{Table: name, Set: []Assignment{{Column: stmt.Name{Raw: "w", Value: "w"}, Value: &Integer{Value: 1}}, {Column: stmt.Name{Raw: "W", Value: "W"}, Value: &Integer{Value: 2}}},
				Source: &Scan{Table: name}}, key: []uint32{1}, want: "an update that sets the column w twice"},
			{plan: &Update{Table: name, Set: []Assignment{{Column: stmt.Name{Raw: "w", Value: "w"}, Value: &Parameter{Number: 1}}}, Source: &Scan{Table: name}},
				key: []uint32{1}, want: "the parameter 1 of a statement with 0 arguments"},
			{plan: &Update{Table: name, Source: &Scan{Table: name}}, key: []uint32{1}, want: "an update that sets no column"},
		} {
			f := newTable()
			f.tables["t"].ct.SetPrimaryKey(c.key)
			_, err := buildSink(t.Context(), fakeTx{f}, c.plan, nil, bugs{}, 1_000_000_000, reading{})
			is.True(t, errors.Is(err, ErrInternal), fmt.Sprint(err))
			is.Equal(t, "s4ql: internal error: "+c.want, err.Error())
		}
	})

	t.Run("should give an internal error for a key that the statement found and whose row is gone", func(t *testing.T) {
		// A source that gives the row of the key 9, from a table of another fake, which the table of the
		// statement does not have.
		f, other := newTable(), newFake().with("t", []string{"w", "k"}, []int64{90, 9})
		other.tables["t"].ct.SetPrimaryKey([]uint32{1})
		ct, src := f.tables["t"].ct, &scan{r: fakeTx{other}, table: other.tables["t"].ct}
		n, err := (&update{w: fakeTx{f}, table: ct, key: 1, src: src, positions: []int{0}, values: []*s4qlpb.Value{integer(5)}}).run(t.Context())
		is.True(t, errors.Is(err, ErrInternal))
		is.Equal(t, "s4ql: internal error: the row of a key that an update found is gone", err.Error())
		is.Equal(t, int64(0), n)
		n, err = (&remove{w: fakeTx{f}, table: ct, key: 1, src: src}).run(t.Context())
		is.True(t, errors.Is(err, ErrInternal))
		is.Equal(t, "s4ql: internal error: the row of a key that a delete found is gone", err.Error())
		is.Equal(t, int64(0), n)
		is.Equal(t, "[keys t add 9 key 9 get t 9 keys t add 9 key 9 delete t 9]", fmt.Sprint(f.calls))
	})
}

func TestBuild(t *testing.T) {
	plan := func(src string) Plan {
		s, err := stmt.Check(src)
		if err != nil {
			t.Fatal(err)
		}
		return NewPlan(s)
	}
	sink := func(f *fake, src string, args ...int64) error {
		var values []*s4qlpb.Value
		for _, a := range args {
			values = append(values, integer(a))
		}
		s, err := buildSink(t.Context(), fakeTx{f}, plan(src), values, bugs{}, 1_000_000_000, reading{})
		if err != nil {
			return err
		}
		_, err = s.run(t.Context())
		return err
	}

	cases := []struct {
		name string
		f    *fake
		src  string
		args []int64
		err  string
		// calls of the storage, after those of the lookups.
		calls string
	}{
		{name: "create table", f: newFake(), src: `create table "T 1" ("V" int primary key) strict, without rowid`,
			calls: "[table T 1 create T 1]"},
		{name: "create table that exists, by a name in another case", f: newFake().with("t1", []string{"v"}), src: `create table "T1" (v int primary key) strict, without rowid`,
			err: `table "T1" already exists`, calls: "[table T1]"},
		{name: "drop table, by its stored name", f: newFake().with("Tb", []string{"v"}), src: "drop table TB",
			calls: "[table TB drop Tb]"},
		{name: "drop table that does not exist", f: newFake(), src: `drop table "x y"`,
			err: "no such table: x y", calls: "[table x y]"},
		{name: "insert", f: newFake().with("t", []string{"V"}), src: "insert into T (v) values (-3)",
			calls: "[table T insert t -3]"},
		{name: "insert of a parameter", f: newFake().with("t", []string{"V"}), src: "insert into T (v) values (?)", args: []int64{-9223372036854775808},
			calls: "[table T insert t -9223372036854775808]"},
		{name: "insert of a parameter into a table that does not exist", f: newFake(), src: "insert into t (v) values (?)", args: []int64{1},
			err: "no such table: t", calls: "[table t]"},
		{name: "insert into a table that does not exist, with a column that does not exist", f: newFake(), src: "insert into t (w) values (1)",
			err: "no such table: t", calls: "[table t]"},
		{name: "insert into a column that does not exist", f: newFake().with("t", []string{"v"}), src: `insert into "T" ("w""") values (1)`,
			err: `table T has no column named w"`, calls: "[table T]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := sink(c.f, c.src, c.args...)
			if c.err == "" {
				is.NotError(t, err)
			} else {
				var se *sqlerr.Error
				is.True(t, errors.As(err, &se))
				is.Equal(t, c.err, se.Message)
				is.Equal(t, 1, se.Code)
				is.Equal(t, 1, se.ExtendedCode)
				// The offset is that of the name for a table that exists, and none for the others.
				want := -1
				if strings.HasSuffix(c.err, "already exists") {
					want = strings.Index(c.src, `"T1"`)
				}
				is.Equal(t, want, se.Offset)
			}
			is.Equal(t, c.calls, fmt.Sprint(c.f.calls))
		})
	}

	t.Run("should place each value of an insert at the position of its column, and look the key up at its position", func(t *testing.T) {
		for _, src := range []string{"insert into t (c, k, a) values (3, ?, 1)", "insert into t values (1, ?, 3)"} {
			f := newFake().with("t", []string{"a", "k", "c"})
			f.tables["t"].ct.SetPrimaryKey([]uint32{1})
			s, err := buildSink(t.Context(), fakeTx{f}, plan(src), []*s4qlpb.Value{integer(2)}, bugs{}, 1_000_000_000, reading{})
			is.NotError(t, err, src)
			is.NotError(t, s.check(t.Context()), src)
			_, err = s.run(t.Context())
			is.NotError(t, err, src)
			is.Equal(t, "[table t insert t 1,2,3]", fmt.Sprint(f.calls), src)
			_, err = s.run(t.Context())
			is.Equal(t, "UNIQUE constraint failed: t.k", err.Error(), src)
		}
	})

	t.Run("should pass on an error of the lookup of an index of the name of a create table, and create nothing", func(t *testing.T) {
		f := newFake()
		f.indexFails = errors.New("the lookup of the index fails")
		err := sink(f, "create table t (v int primary key) strict, without rowid")
		is.True(t, errors.Is(err, f.indexFails))
		is.Equal(t, "[table t]", fmt.Sprint(f.calls))
	})

	t.Run("should pass on an error of the lookup of a table and of an index of a create index and a drop index, and create and drop nothing", func(t *testing.T) {
		f := newFake().with("t", []string{"k", "v"})
		f.tableFails = errors.New("the lookup of the table fails")
		err := sink(f, "create index i on t (v)")
		is.True(t, errors.Is(err, f.tableFails))
		f.tableFailsFor = "I"
		err = sink(f, "create index i on t (v)")
		is.True(t, errors.Is(err, f.tableFails), "the lookup of a table of the name of the index")

		f = newFake().with("t", []string{"k", "v"}).index("i", "t", 1)
		f.indexFails = errors.New("the lookup of the index fails")
		for _, src := range []string{"create index j on t (v)", "drop index i"} {
			err := sink(f, src)
			is.True(t, errors.Is(err, f.indexFails), src)
		}
		is.Equal(t, "[table t table j]", fmt.Sprint(f.calls))
	})

	t.Run("should place the values of a list in the order of the list with the planted bug listOrder", func(t *testing.T) {
		f := newFake().with("t", []string{"a", "k", "c"})
		f.tables["t"].ct.SetPrimaryKey([]uint32{1})
		s, err := buildSink(t.Context(), fakeTx{f}, plan("insert into t (c, k, a) values (3, ?, 1)"), []*s4qlpb.Value{integer(2)}, bugs{listOrder: true}, 1_000_000_000, reading{})
		is.NotError(t, err)
		is.NotError(t, s.check(t.Context()))
		_, err = s.run(t.Context())
		is.NotError(t, err)
		is.Equal(t, "[table t insert t 3,2,1]", fmt.Sprint(f.calls))
	})

	t.Run("should give NULL to a column that the list leaves out, and a NULL literal or argument to its column", func(t *testing.T) {
		for src, arg := range map[string]*s4qlpb.Value{
			"insert into t (k) values (?)":          integer(2),
			"insert into t (c, k) values (null, ?)": integer(2),
			"insert into t values (?, 2, null)":     null(),
			"insert into t (k, a) values (2, ?)":    null(),
		} {
			f := newFake().with("t", []string{"a", "k", "c"})
			ct := f.tables["t"].ct
			ct.SetPrimaryKey([]uint32{1})
			ct.GetColumns()[0].SetNullable(true)
			ct.GetColumns()[2].SetNullable(true)
			s, err := buildSink(t.Context(), fakeTx{f}, plan(src), []*s4qlpb.Value{arg}, bugs{}, 1_000_000_000, reading{})
			is.NotError(t, err, src)
			is.NotError(t, s.check(t.Context()), src)
			_, err = s.run(t.Context())
			is.NotError(t, err, src)
			is.Equal(t, "[table t insert t null,2,null]", fmt.Sprint(f.calls), src)
		}
	})

	t.Run("should give the zero value of its type to a column that takes NULL and that the list leaves out, with the planted bug zeroLeftOut", func(t *testing.T) {
		text := func(row []*s4qlpb.Value) string {
			var values []string
			for _, v := range row {
				switch {
				case v.HasNull():
					values = append(values, "null")
				case v.HasReal():
					values = append(values, fmt.Sprint("real ", v.GetReal()))
				case v.HasText():
					values = append(values, strconv.Quote(v.GetText()))
				case v.HasBlob():
					values = append(values, fmt.Sprintf("x'%x' nil %v", v.GetBlob(), v.GetBlob() == nil))
				default:
					values = append(values, fmt.Sprint(v.GetInteger()))
				}
			}
			return strings.Join(values, ",")
		}
		for typ, zero := range map[s4qlpb.Type]string{
			s4qlpb.Type_TYPE_INT: "0", s4qlpb.Type_TYPE_INTEGER: "0", s4qlpb.Type_TYPE_REAL: "real 0", s4qlpb.Type_TYPE_TEXT: `""`, s4qlpb.Type_TYPE_BLOB: "x'' nil false",
		} {
			// A NULL that the insert gives stays NULL, and so does a column without a list.
			for src, want := range map[string]string{
				"insert into t (k) values (2)":          zero + ",2," + zero,
				"insert into t (k, c) values (2, null)": zero + ",2,null",
				"insert into t values (null, 2, null)":  "null,2,null",
			} {
				f := newFake().with("t", []string{"a", "k", "c"})
				ct := f.tables["t"].ct
				ct.SetPrimaryKey([]uint32{1})
				for _, position := range []int{0, 2} {
					ct.GetColumns()[position].SetNullable(true)
					ct.GetColumns()[position].SetType(typ)
				}
				s, err := buildSink(t.Context(), fakeTx{f}, plan(src), nil, bugs{zeroLeftOut: true}, 1_000_000_000, reading{})
				is.NotError(t, err, src)
				is.NotError(t, s.check(t.Context()), src)
				is.Equal(t, want, text(s.(*insert).src.data[0]), src)
			}
		}
	})

	t.Run("should check that each column has a value before the type of each value, in the order of the table", func(t *testing.T) {
		f := newFake().with("t", []string{"a", "k", "c"})
		f.tables["t"].ct.SetPrimaryKey([]uint32{1})
		for src, want := range map[string]string{
			"insert into t (c, a) values ('x', 'y')":       "NOT NULL constraint failed: t.k",
			"insert into t (k, a) values ('x', 1)":         "NOT NULL constraint failed: t.c",
			"insert into t (c, a, k) values (1, 'y', 'x')": "s4ql: not supported: a string literal for the column t.a of type INT",
		} {
			f.calls = nil
			s, err := buildSink(t.Context(), fakeTx{f}, plan(src), nil, bugs{}, 1_000_000_000, reading{})
			is.NotError(t, err, src)
			is.Equal(t, want, s.check(t.Context()).Error(), src)
			is.Equal(t, "[table t]", fmt.Sprint(f.calls), src)
		}
	})

	t.Run("should build a query of a table that exists, and give SQLite's error for one that does not", func(t *testing.T) {
		f := newFake().with("t", []string{"v"}, []int64{2}, []int64{1})
		src, err := buildQuery(t.Context(), fakeTx{f}, plan("select * from T").(*Query), nil, 1_000_000_000, reading{})
		is.NotError(t, err)
		rows, err := collect(t.Context(), src)
		is.NotError(t, err)
		is.Equal(t, "[1 2]", fmt.Sprint(rows))
		_, err = buildQuery(t.Context(), fakeTx{f}, plan("select * from u").(*Query), nil, 1_000_000_000, reading{})
		is.Equal(t, "no such table: u", err.Error())
	})

	t.Run("should fail with an internal error for plans that no supported statement gives", func(t *testing.T) {
		one := [][]Expr{{&Integer{Value: 1}}}
		v := []stmt.Name{{Value: "v"}}
		ab := []ColumnDef{{Name: stmt.Name{Value: "a"}}, {Name: stmt.Name{Value: "b"}}}
		cases := []struct {
			name string
			plan Plan
			err  string
		}{
			{name: "a query of VALUES", plan: &Query{Root: &Values{Rows: one}}, err: "a query of a node of type *engine.Values"},
			{name: "a projection of no columns", plan: &Query{Root: &Project{Source: &Scan{Table: stmt.Name{Value: "t"}}}}, err: "a projection of no columns"},
			{name: "a projection of VALUES", plan: &Query{Root: &Project{Source: &Values{Rows: one}, Star: true}}, err: "a query of a node of type *engine.Values"},
			{name: "an insert from a table", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: v, Source: &Scan{Table: stmt.Name{Value: "u"}}}, err: "an insert from a node of type *engine.Scan"},
			{name: "an insert of two rows", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: v, Source: &Values{Rows: [][]Expr{{&Integer{Value: 1}}, {&Integer{Value: 2}}}}}, err: "an insert of 2 rows"},
			{name: "an insert of no rows", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: v, Source: &Values{}}, err: "an insert of 0 rows"},
			{name: "an insert into a table without a key", plan: &Insert{Table: stmt.Name{Value: "nokey"}, Columns: v, Source: &Values{Rows: one}}, err: "an insert into a table of 1 columns with the key []"},
			{name: "an insert into a table with a key of two columns", plan: &Insert{Table: stmt.Name{Value: "ab"}, Columns: []stmt.Name{{Value: "a"}}, Source: &Values{Rows: one}}, err: "an insert into a table of 2 columns with the key [0 1]"},
			{name: "an insert into a table with a key outside its columns", plan: &Insert{Table: stmt.Name{Value: "far"}, Columns: v, Source: &Values{Rows: one}}, err: "an insert into a table of 1 columns with the key [1]"},
			{name: "an insert into one column twice", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: []stmt.Name{{Value: "v"}, {Value: "V"}}, Source: &Values{Rows: [][]Expr{{&Integer{Value: 1}, &Integer{Value: 2}}}}}, err: "an insert that names the column v twice"},
			{name: "a plan of a type that the engine does not know", plan: unknownPlan{}, err: "a plan of type engine.unknownPlan"},
			{name: "a table with a key of two columns", plan: &CreateTable{Table: stmt.Name{Value: "u"}, Columns: ab, PrimaryKey: []int{0, 1}}, err: "a table of 2 columns with the key [0 1]"},
			{name: "a table with a key outside its columns", plan: &CreateTable{Table: stmt.Name{Value: "u"}, Columns: ab, PrimaryKey: []int{2}}, err: "a table of 2 columns with the key [2]"},
			{name: "a table with a negative key", plan: &CreateTable{Table: stmt.Name{Value: "u"}, Columns: ab, PrimaryKey: []int{-1}}, err: "a table of 2 columns with the key [-1]"},
			{name: "an insert of a parameter without its argument", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: v, Source: &Values{Rows: [][]Expr{{&Parameter{Number: 1}}}}}, err: "the parameter 1 of a statement with 0 arguments"},
			{name: "an insert of the parameter 0", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: v, Source: &Values{Rows: [][]Expr{{&Parameter{Number: 0}}}}}, err: "the parameter 0 of a statement with 0 arguments"},
			{name: "a table without a key", plan: &CreateTable{Table: stmt.Name{Value: "u"}, Columns: []ColumnDef{{Name: stmt.Name{Value: "a"}}}}, err: "a table of 1 columns with the key []"},
			{name: "a table whose key takes NULL", plan: &CreateTable{Table: stmt.Name{Value: "u"}, Columns: []ColumnDef{{Name: stmt.Name{Value: "a"}}, {Name: stmt.Name{Value: "b"}, Nullable: true}}, PrimaryKey: []int{1}}, err: "a table whose key b takes NULL"},
			{name: "an insert of a blob literal that is nil", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: v, Source: &Values{Rows: [][]Expr{{&Blob{}}}}}, err: "a blob literal of no bytes that is nil"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				f := newFake().with("t", []string{"v"}, []int64{1}).with("ab", []string{"a", "b"}).with("nokey", []string{"v"}).with("far", []string{"v"})
				f.tables["nokey"].ct.SetPrimaryKey(nil)
				f.tables["ab"].ct.SetPrimaryKey([]uint32{0, 1})
				f.tables["far"].ct.SetPrimaryKey([]uint32{1})
				var err error
				if q, ok := c.plan.(*Query); ok {
					_, err = buildQuery(t.Context(), fakeTx{f}, q, nil, 1_000_000_000, reading{})
				} else {
					_, err = buildSink(t.Context(), fakeTx{f}, c.plan, nil, bugs{}, 1_000_000_000, reading{})
				}
				is.True(t, errors.Is(err, ErrInternal))
				is.Equal(t, "s4ql: internal error: "+c.err, err.Error())
			})
		}
	})

	t.Run("should pass on an error of a lookup", func(t *testing.T) {
		for _, src := range []string{t1, "drop table t", "insert into t (v) values (1)", "select * from t"} {
			var err error
			if q, ok := plan(src).(*Query); ok {
				_, err = buildQuery(t.Context(), failingLookUp{}, q, nil, 1_000_000_000, reading{})
			} else {
				_, err = buildSink(t.Context(), failingLookUp{}, plan(src), nil, bugs{}, 1_000_000_000, reading{})
			}
			is.Equal(t, "the lookup fails", err.Error(), src)
		}
	})
}

const t1 = "create table t1 (v1 int primary key) strict, without rowid"

// integer value v.
func integer(v int64) *s4qlpb.Value {
	return s4qlpb.Value_builder{Integer: new(v)}.Build()
}

// unknownPlan is a plan of a type that the engine does not know.
type unknownPlan struct{}

func (unknownPlan) plan() {}

// failingLookUp is a writer whose lookups fail.
type failingLookUp struct {
	writer
}

func (failingLookUp) table(context.Context, string) (*s4qlpb.CreateTable, error) {
	return nil, errors.New("the lookup fails")
}

// readingOf a statement of a test: of every row of its table if scanAll, and as the rules choose
// otherwise, forward.
func readingOf(scanAll bool) reading {
	if scanAll {
		return reading{path: path{kind: pathFull}}
	}
	return reading{}
}
