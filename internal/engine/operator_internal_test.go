package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
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
	// getFails, upsertFails, annotateFails, and commitFails are the errors of those calls.
	getFails, upsertFails, annotateFails, commitFails, beginFails, readFails error
	// upsertPanics makes an upsert panic, and endPanics the end of a read transaction.
	upsertPanics, endPanics bool
	// scanPanics makes a scan panic before the row of that index, if it is not negative.
	scanPanics int
	// tablePanics makes a lookup panic, tableFails is the error of a lookup, and endFails is the
	// error of the end of a read transaction.
	tablePanics bool
	tableFails  error
	endFails    error
	// scans counts the scans that have started and not ended, and open the transactions.
	scans, open int
	// readers counts the readers that are open, and opened those that were ever opened. readerFails
	// is the error of opening one, and closeFails of closing one.
	readers, opened         int
	readerFails, closeFails error
	// calls of the transactions, in order.
	calls []string
}

type fakeTable struct {
	ct   *s4qlpb.CreateTable
	rows [][]*s4qlpb.Value
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
	if x.f.tableFails != nil {
		return nil, x.f.tableFails
	}
	if t, ok := x.f.tables[fold(name)]; ok {
		return t.ct, nil
	}
	return nil, nil
}

func (x fakeTx) scan(_ context.Context, name string) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		x.f.scans++
		defer func() { x.f.scans-- }()
		t := x.f.tables[fold(name)]
		rows := slices.Clone(t.rows)
		slices.SortFunc(rows, func(a, b []*s4qlpb.Value) int { return cmp.Compare(a[0].GetInteger(), b[0].GetInteger()) })
		for i, row := range rows {
			if i == x.f.scanPanics {
				panic("the scan panics")
			}
			if i == x.f.scanFails {
				yield(nil, errors.New("the scan fails"))
				return
			}
			if !yield(row, nil) {
				return
			}
		}
	}
}

func (x fakeTx) get(_ context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	x.f.calls = append(x.f.calls, fmt.Sprint("get ", name, " ", key[0].GetInteger()))
	if x.f.getFails != nil {
		return nil, x.f.getFails
	}
	for _, row := range x.f.tables[fold(name)].rows {
		if row[0].GetInteger() == key[0].GetInteger() {
			return row, nil
		}
	}
	return nil, nil
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
	if x.f.upsertPanics {
		panic("the upsert panics")
	}
	var values []string
	for _, v := range row {
		if v.HasNull() {
			values = append(values, "null")
		} else {
			values = append(values, fmt.Sprint(v.GetInteger()))
		}
	}
	x.f.calls = append(x.f.calls, fmt.Sprint("upsert ", name, " ", strings.Join(values, ",")))
	if x.f.upsertFails != nil {
		return x.f.upsertFails
	}
	t := x.f.tables[fold(name)]
	t.rows = slices.DeleteFunc(t.rows, func(r []*s4qlpb.Value) bool { return r[0].GetInteger() == row[0].GetInteger() })
	t.rows = append(t.rows, row)
	return nil
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

func TestInsert(t *testing.T) {
	ints := func(vs ...int64) *values {
		v := &values{}
		for _, x := range vs {
			v.data = append(v.data, []*s4qlpb.Value{integer(x)})
		}
		return v
	}

	t.Run("should check each key before it writes the row, and count the rows", func(t *testing.T) {
		f := newFake().with("T1", []string{"v"})
		i := &insert{w: fakeTx{f}, table: f.tables["t1"].ct, src: ints(2, 1)}
		n, err := i.run(t.Context())
		is.NotError(t, err)
		is.Equal(t, int64(2), n)
		is.Equal(t, "[get T1 2 upsert T1 2 get T1 1 upsert T1 1]", fmt.Sprint(f.calls))
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
		f.getFails = errors.New("get fails")
		_, err := (&insert{w: fakeTx{f}, table: f.tables["t"].ct, src: ints(1)}).run(t.Context())
		is.Equal(t, "get fails", err.Error())
		f.getFails, f.upsertFails = nil, errors.New("upsert fails")
		_, err = (&insert{w: fakeTx{f}, table: f.tables["t"].ct, src: ints(1)}).run(t.Context())
		is.Equal(t, "upsert fails", err.Error())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = (&insert{w: fakeTx{f}, table: f.tables["t"].ct, src: ints(1)}).run(ctx)
		is.True(t, errors.Is(err, context.Canceled))
	})

	t.Run("should write without the check with the planted bug", func(t *testing.T) {
		f := newFake().with("t", []string{"v"}, []int64{1})
		_, err := (&insert{w: fakeTx{f}, table: f.tables["t"].ct, src: ints(1), skipUnique: true}).run(t.Context())
		is.NotError(t, err)
		is.Equal(t, "[upsert t 1]", fmt.Sprint(f.calls))
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
		s, err := buildSink(t.Context(), fakeTx{f}, plan(src), values, bugs{})
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
			calls: "[table T get t -3 upsert t -3]"},
		{name: "insert of a parameter", f: newFake().with("t", []string{"V"}), src: "insert into T (v) values (?)", args: []int64{-9223372036854775808},
			calls: "[table T get t -9223372036854775808 upsert t -9223372036854775808]"},
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

	t.Run("should build a query of a table that exists, and give SQLite's error for one that does not", func(t *testing.T) {
		f := newFake().with("t", []string{"v"}, []int64{2}, []int64{1})
		src, err := buildQuery(t.Context(), fakeTx{f}, plan("select * from T").(*Query))
		is.NotError(t, err)
		rows, err := collect(t.Context(), src)
		is.NotError(t, err)
		is.Equal(t, "[1 2]", fmt.Sprint(rows))
		_, err = buildQuery(t.Context(), fakeTx{f}, plan("select * from u").(*Query))
		is.Equal(t, "no such table: u", err.Error())
	})

	t.Run("should fail with an internal error for plans that no statement of slice 1 gives", func(t *testing.T) {
		one := [][]Expr{{&Integer{Value: 1}}}
		v := []stmt.Name{{Value: "v"}}
		cases := []struct {
			name string
			plan Plan
			err  string
		}{
			{name: "a query of VALUES", plan: &Query{Root: &Values{Rows: one}}, err: "a query of a node of type *engine.Values"},
			{name: "a projection without *", plan: &Query{Root: &Project{Source: &Scan{Table: stmt.Name{Value: "t"}}}}, err: "a projection without *"},
			{name: "a projection of VALUES", plan: &Query{Root: &Project{Source: &Values{Rows: one}, Star: true}}, err: "a query of a node of type *engine.Values"},
			{name: "an insert from a table", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: v, Source: &Scan{Table: stmt.Name{Value: "u"}}}, err: "an insert from a node of type *engine.Scan"},
			{name: "an insert of two rows", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: v, Source: &Values{Rows: [][]Expr{{&Integer{Value: 1}}, {&Integer{Value: 2}}}}}, err: "an insert of 2 rows"},
			{name: "an insert of no rows", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: v, Source: &Values{}}, err: "an insert of 0 rows"},
			{name: "an insert of two values", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: v, Source: &Values{Rows: [][]Expr{{&Integer{Value: 1}, &Integer{Value: 2}}}}}, err: "an insert of 2 values"},
			{name: "an insert into a table of two columns", plan: &Insert{Table: stmt.Name{Value: "ab"}, Columns: []stmt.Name{{Value: "a"}}, Source: &Values{Rows: one}}, err: "an insert of 1 columns into a table of 2 columns with the key [0]"},
			{name: "an insert into a table without a key", plan: &Insert{Table: stmt.Name{Value: "nokey"}, Columns: v, Source: &Values{Rows: one}}, err: "an insert of 1 columns into a table of 1 columns with the key []"},
			{name: "an insert into two columns", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: []stmt.Name{{Value: "v"}, {Value: "V"}}, Source: &Values{Rows: one}}, err: "an insert of 2 columns into a table of 1 columns with the key [0]"},
			{name: "a plan of a type that the engine does not know", plan: unknownPlan{}, err: "a plan of type engine.unknownPlan"},
			{name: "a table of two columns", plan: &CreateTable{Table: stmt.Name{Value: "u"}, Columns: []ColumnDef{{Name: stmt.Name{Value: "a"}}, {Name: stmt.Name{Value: "b"}}}, PrimaryKey: []int{0}}, err: "a table of 2 columns with the key [0]"},
			{name: "an insert of a parameter without its argument", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: v, Source: &Values{Rows: [][]Expr{{&Parameter{Number: 1}}}}}, err: "the parameter 1 of a statement with 0 arguments"},
			{name: "an insert of the parameter 0", plan: &Insert{Table: stmt.Name{Value: "t"}, Columns: v, Source: &Values{Rows: [][]Expr{{&Parameter{Number: 0}}}}}, err: "the parameter 0 of a statement with 0 arguments"},
			{name: "a table without a key", plan: &CreateTable{Table: stmt.Name{Value: "u"}, Columns: []ColumnDef{{Name: stmt.Name{Value: "a"}}}}, err: "a table of 1 columns with the key []"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				f := newFake().with("t", []string{"v"}, []int64{1}).with("ab", []string{"a", "b"}).with("nokey", []string{"v"})
				f.tables["nokey"].ct.SetPrimaryKey(nil)
				var err error
				if q, ok := c.plan.(*Query); ok {
					_, err = buildQuery(t.Context(), fakeTx{f}, q)
				} else {
					_, err = buildSink(t.Context(), fakeTx{f}, c.plan, nil, bugs{})
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
				_, err = buildQuery(t.Context(), failingLookUp{}, q)
			} else {
				_, err = buildSink(t.Context(), failingLookUp{}, plan(src), nil, bugs{})
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
