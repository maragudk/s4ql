package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestSubqueries(t *testing.T) {
	// subsOf gives the subqueries of the select src.
	subsOf := func(t *testing.T, src string) *subqueries {
		t.Helper()
		s, err := stmt.Check(src)
		is.NotError(t, err, src)
		return newSubqueries(NewPlan(s).(*Query).Subqueries)
	}
	// newTables gives a fake with the table t of the columns w and k, whose primary key is k, and the
	// table u of the columns a and b.
	newTables := func() *fake {
		f := newFake().with("t", []string{"w", "k"}, []int64{10, 1}, []int64{20, 2})
		f.tables["t"].ct.SetPrimaryKey([]uint32{1})
		return f.with("u", []string{"a", "b"}, []int64{1, 2}, []int64{3, 4})
	}
	internal := func(t *testing.T, want string, err error) {
		t.Helper()
		is.True(t, errors.Is(err, ErrInternal), want)
		is.Equal(t, "s4ql: internal error: "+want, fmt.Sprint(err))
	}
	// panicOf f, or nil if it does not panic.
	panicOf := func(f func()) (v any) {
		defer func() { v = recover() }()
		f()
		return nil
	}

	t.Run("should be nil and count none for a statement without subqueries", func(t *testing.T) {
		is.True(t, newSubqueries(nil) == nil)
		is.Equal(t, 0, (*subqueries)(nil).count())
	})

	t.Run("should panic for a subquery of another shape than a plan of a select gives", func(t *testing.T) {
		scan := &Scan{Table: stmt.Name{Value: "u"}}
		for _, c := range []struct {
			q    *Query
			want string
		}{
			{q: &Query{Root: scan}, want: "engine: a subquery of a root of type *engine.Scan"},
			{q: &Query{Root: &Project{Source: scan}, Subqueries: []*Query{{Root: &Project{Source: scan}}}}, want: "engine: a subquery of a root of type *engine.Project"},
			{q: &Query{Root: &Project{Source: &Values{}}}, want: "engine: a subquery of a node of type *engine.Values"},
		} {
			is.Equal(t, any(c.want), panicOf(func() { newSubqueries([]*Query{c.q}) }))
		}
		is.Equal(t, any("engine: a subquery with subqueries"), panicOf(func() { subqueriesOf([]stmt.Select{{Subqueries: []stmt.Select{{}}}}) }))
	})

	t.Run("should give an internal error for a subquery that the statement does not have, and for a condition of a subquery", func(t *testing.T) {
		s := subsOf(t, "select * from t where k = (select a from u)")
		in, err := s.at(&Subquery{Number: 1}, nil)
		is.NotError(t, err)
		is.True(t, in == s.list[0])
		outer := newTables().tables["t"].ct
		for _, c := range []struct {
			s      *subqueries
			number int
			outer  *s4qlpb.CreateTable
			want   string
		}{
			{number: 1, want: "the subquery 1 of a statement with 0"},
			{s: s, number: 0, want: "the subquery 0 of a statement with 1"},
			{s: s, number: 2, want: "the subquery 2 of a statement with 1"},
			{s: s, number: 1, outer: outer, want: "the subquery 1 of a statement with 1"},
		} {
			_, err := c.s.at(&Subquery{Number: c.number}, c.outer)
			internal(t, c.want, err)
		}
		internal(t, "the subquery 2 of a statement with 1", s.lengths(build{}, &Subquery{Number: 2}))
		internal(t, "the subquery 2 of a statement with 1", s.types(build{}, &Subquery{Number: 2}, outer, 0, false, classAny))
		f := &Filter{Conditions: []Condition{{Column: stmt.Name{Value: "k"}, Operator: stmt.OpEqual, Value: &Subquery{Number: 2}}}}
		internal(t, "the subquery 2 of a statement with 1", build{r: fakeTx{newTables()}, subs: s}.compute(t.Context(), f, &scan{table: outer}, make([]condition, 1), false))
	})

	t.Run("should find the table of a subquery once, with its width, and pass on the error of the lookup", func(t *testing.T) {
		f := newTables()
		s := subsOf(t, "select * from t where k = (select a from u) and w = (select * from u)")
		for _, in := range s.list {
			is.NotError(t, in.find(t.Context(), fakeTx{f}))
			is.NotError(t, in.find(t.Context(), fakeTx{f}))
		}
		is.Equal(t, "[table u table u]", fmt.Sprint(f.calls))
		is.Equal(t, 1, s.list[0].width)
		is.Equal(t, 2, s.list[1].width)

		f = newTables()
		f.tableFails, f.tableFailsFor = errors.New("the lookup fails"), "u"
		s = subsOf(t, "select * from t where k = (select a from u)")
		is.Equal(t, "the lookup fails", fmt.Sprint(s.list[0].find(t.Context(), fakeTx{f})))
		is.True(t, s.list[0].s == nil)
	})

	t.Run("should give -1 and true for a correlated name and record the first, and 0 and false for a name that neither table has", func(t *testing.T) {
		f := newTables()
		of, outer := columnPositions(f.tables["u"].ct), f.tables["t"].ct
		s := &subqueries{}
		for _, c := range []struct {
			name     string
			outer    *s4qlpb.CreateTable
			position int
			ok       bool
		}{
			{name: "b", outer: outer, position: 1, ok: true},
			{name: "w", outer: outer, position: -1, ok: true},
			{name: "k", outer: outer, position: -1, ok: true},
			{name: "x", outer: outer, position: 0, ok: false},
			{name: "w", position: 0, ok: false},
		} {
			position, ok := s.lookup(of, c.outer, stmt.Name{Value: c.name})
			is.Equal(t, c.position, position, c.name)
			is.Equal(t, c.ok, ok, c.name)
		}
		is.Equal(t, "w", s.correlated.Value)
	})

	t.Run("should give an internal error for the names of a subquery looked up twice, and for a subquery of two columns", func(t *testing.T) {
		f := newTables()
		s := subsOf(t, "select * from t where k = (select a from u where b = 2)")
		b := build{r: fakeTx{f}, subs: s}
		is.NotError(t, s.names(t.Context(), b, s.list[0]))
		internal(t, "the names of the subquery 1 twice", s.names(t.Context(), b, s.list[0]))

		s = newSubqueries([]*Query{{Root: &Project{Columns: []stmt.Name{{Value: "a"}, {Value: "b"}}, Source: &Scan{Table: stmt.Name{Value: "u"}}}}})
		internal(t, "a subquery of 2 columns", s.names(t.Context(), build{r: fakeTx{f}, subs: s}, s.list[0]))
	})

	t.Run("should give an internal error for a subquery of a column that its table or its rows do not have, or compared with a column that the table of the statement does not have", func(t *testing.T) {
		f := newTables()
		s := subsOf(t, "select * from t where k = (select a from u)")
		in := s.list[0]
		is.NotError(t, s.names(t.Context(), build{r: fakeTx{f}, subs: s}, in))
		outer := f.tables["t"].ct
		is.NotError(t, s.types(build{}, &Subquery{Number: 1}, outer, 1, false, classAny))
		for _, c := range []struct{ inside, outside int }{{-1, 0}, {2, 0}, {0, -1}, {0, 2}} {
			in.position = c.inside
			internal(t, fmt.Sprintf("a subquery of the column at %v compared with the column at %v", c.inside, c.outside), s.types(build{}, &Subquery{Number: 1}, outer, c.outside, false, classAny))
		}
		// The same compared with an expression that the condition tests, k + 1.
		_, err := bindExpression(outer, stmt.Expression{Kind: stmt.ExprAdd, Operands: []stmt.Expression{{Kind: stmt.ExprColumn, Column: stmt.Name{Value: "k"}}, {Kind: stmt.ExprValue, Value: stmt.Integer(1)}}}, nil, 1000)
		is.NotError(t, err)
		for _, position := range []int{-1, 2} {
			in.position = position
			internal(t, fmt.Sprintf("a subquery of the column at %v compared with an expression", position), s.types(build{}, &Subquery{Number: 1}, outer, -1, true, classInteger))
		}
		for _, position := range []int{-1, 2} {
			in.position = position
			_, _, err := build{r: fakeTx{f}}.value(t.Context(), in)
			internal(t, fmt.Sprintf("a subquery of the column at %v of a row of 2 values", position), err)
		}
	})

	t.Run("should compute nothing and observe nothing for a statement without subqueries", func(t *testing.T) {
		b := build{how: reading{observe: func(context.Context, []*s4qlpb.Value) []*s4qlpb.Value {
			t.Error("observed")
			return nil
		}}}
		is.NotError(t, b.compute(t.Context(), &Filter{}, nil, nil, false))
	})

	t.Run("should give no offset for SQLite's errors of a subquery of more than one column in an update or a delete", func(t *testing.T) {
		s := &subqueries{list: []*inner{{width: 2}}}
		tested := stmt.Expression{Kind: stmt.ExprAdd, Operands: []stmt.Expression{{Kind: stmt.ExprColumn, Column: stmt.Name{Value: "k"}}, {Kind: stmt.ExprValue, Value: stmt.Integer(1)}}}
		for _, c := range []struct {
			c    Condition
			want string
		}{
			{c: Condition{Operator: stmt.OpNotEqual, ValueFirst: true, Value: &Subquery{Number: 1}}, want: "row value misused"},
			{c: Condition{Operator: stmt.OpEqual, Value: &Subquery{Number: 1}}, want: "sub-select returns 2 columns - expected 1"},
			{c: Condition{Operator: stmt.OpLess, ValueFirst: true, Value: &Subquery{Number: 1}}, want: "sub-select returns 2 columns - expected 1"},
			{c: Condition{Expression: tested, Operator: stmt.OpLess, ValueFirst: true, Value: &Subquery{Number: 1}}, want: "row value misused"},
			{c: Condition{Expression: tested, Operator: stmt.OpEqual, ValueFirst: true, Value: &Subquery{Number: 1}}, want: "row value misused"},
			{c: Condition{Expression: tested, Operator: stmt.OpEqual, Value: &Subquery{Number: 1}}, want: "sub-select returns 2 columns - expected 1"},
			{c: Condition{Expression: tested, Operator: stmt.OpNotEqual, Value: &Subquery{Number: 1}}, want: "sub-select returns 2 columns - expected 1"},
		} {
			var se *sqlerr.Error
			is.True(t, errors.As(s.widths(&Filter{Conditions: []Condition{c.c}}), &se), c.want)
			is.Equal(t, c.want, se.Message)
			is.Equal(t, -1, se.Offset, c.want)
		}
	})

	t.Run("should name an access of no row none, and give the indexes and the orders of the subqueries where any has one", func(t *testing.T) {
		is.Equal(t, "none", readOf(access{none: true}).access)
		r := &report{read: []subqueryRead{{access: "index", index: "i", order: "delivered", rows: 1, found: true}, {access: "full_scan", rows: 2}}}
		is.Equal(t, "[statement.subqueries.found=1 statement.subqueries.rows_read=3 statement.subqueries.access=index,full_scan statement.subqueries.index=i, statement.subqueries.order=delivered,]", fmt.Sprint(r.subqueryAttrs()))
	})

	t.Run("should read no index for a subquery without WHERE and ORDER BY, and pass on the errors of the read of a subquery and of its LIMIT", func(t *testing.T) {
		// build src, whose statement reads by the whole key of t and so looks no index up.
		build := func(f *fake, src string) error {
			s, err := stmt.Check(src)
			is.NotError(t, err, src)
			_, err = buildQuery(t.Context(), fakeTx{f}, NewPlan(s).(*Query), nil, 1_000_000_000, reading{})
			return err
		}
		f := newTables()
		is.NotError(t, build(f, "select * from t where k = (select a from u)"))
		is.Equal(t, "[table t table u]", fmt.Sprint(f.calls))

		f = newTables()
		f.indexesFails = errors.New("the read of the indexes fails")
		is.Equal(t, "the read of the indexes fails", fmt.Sprint(build(f, "select * from t where k = (select a from u where b = 2)")))

		f = newTables()
		f.scanFails = 0
		is.Equal(t, "the scan fails", fmt.Sprint(build(f, "select * from t where k = (select a from u)")))

		internal(t, "the parameter 1 of a statement with 0 arguments", build(newTables(), "select * from t where k = (select a from u limit ?)"))
	})
}
