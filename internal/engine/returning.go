package engine

import (
	"context"
	"iter"
	"math"
	"slices"

	"maragu.dev/s4ql/internal/s4qlpb"
)

// returning holds the rows that an insert, an update, or a delete with RETURNING gives: for each row
// that the statement inserts, updates, or deletes, the values of the columns of its RETURNING, as
// the table stores them. Its sink adds each row once it has written it, so a row is held only for a
// change that succeeded, and the statement drops them all if it fails. The rows are held in memory,
// so a statement that changes many rows holds as many; see the todo of the storage core.
type returning struct {
	// table whose rows the statement changes, for the columns of the rows that it gives.
	table *s4qlpb.CreateTable
	// positions of the columns of RETURNING in the table, in their order, each as often as the clause
	// names it.
	positions []int
	// rows that the statement gives, in the order in which it changed them.
	rows [][]*s4qlpb.Value
}

// buildReturning for a statement with the RETURNING r on the table t: the positions of its columns,
// which SQLite resolves in the order of the text once it has resolved every other name of the
// statement, with SQLite's `no such column` for the first that the table does not have, as for a
// select list. It gives nil for a statement without RETURNING. The bugs b are those planted in the
// statement.
func buildReturning(t *s4qlpb.CreateTable, r *Returning, b bugs) (*returning, error) {
	if r == nil {
		return nil, nil
	}
	ret := &returning{table: t}
	columns := t.GetColumns()
	switch {
	case r.Star && b.starKeyFirst:
		// The planted bug: the key first, and then the other columns in the order of the table.
		key := int(t.GetPrimaryKey()[0])
		ret.positions = append(ret.positions, key)
		for i := range columns {
			if i != key {
				ret.positions = append(ret.positions, i)
			}
		}
	case r.Star:
		for i := range columns {
			ret.positions = append(ret.positions, i)
		}
	case len(r.Columns) == 0:
		return nil, internalError("a RETURNING of no columns")
	default:
		names := make([]string, len(columns))
		for i, c := range columns {
			names[i] = c.GetName()
		}
		of := positionsOf(names)
		for _, name := range r.Columns {
			position, ok := of[name.Fold()]
			if !ok {
				return nil, noSuchResultColumn(name)
			}
			ret.positions = append(ret.positions, position)
		}
	}
	return ret, nil
}

// add the row, which the statement has written into its table or deleted from it, with a value for
// each column of the table, to the rows that it gives. A real that is -0.0 is given as 0.0, which
// is what the table stores for it. It does nothing on a nil returning, of a statement without
// RETURNING.
func (r *returning) add(row []*s4qlpb.Value) {
	if r == nil {
		return
	}
	out := make([]*s4qlpb.Value, len(r.positions))
	for i, position := range r.positions {
		v := row[position]
		if f := v.GetReal(); v.HasReal() && f == 0 && math.Signbit(f) {
			v = s4qlpb.Value_builder{Real: new(0.0)}.Build()
		}
		out[i] = v
	}
	r.rows = append(r.rows, out)
}

// columns of the rows: the names as the table stores them, and the declared types, as a select of
// the same columns gives them.
func (r *returning) columns() []Column {
	all := r.table.GetColumns()
	cs := make([]Column, len(r.positions))
	for i, position := range r.positions {
		c := all[position]
		cs[i] = Column{Name: c.GetName(), DeclType: typeName(c.GetType())}
	}
	return cs
}

// returnsOf the sink s: the rows of its RETURNING, or nil for a statement without one.
func returnsOf(s sink) *returning {
	switch s := s.(type) {
	case *insert:
		return s.ret
	case *update:
		return s.ret
	case *remove:
		return s.ret
	}
	return nil
}

// dropReturns of the sink s, whose rows of RETURNING no one takes, as for [Conn.Exec]: it holds none
// of them, and a delete reads no row again to give its values. The columns of its RETURNING are
// resolved all the same, when the sink is built, so the statement fails as through [Conn.Query].
func dropReturns(s sink) {
	switch s := s.(type) {
	case *insert:
		s.ret = nil
	case *update:
		s.ret = nil
	case *remove:
		s.ret = nil
	}
}

// returned is the [source] of the rows of a statement with RETURNING that has run, which the engine
// holds: the rows in the order in which the statement changed them, which is the order of the
// primary key of the rows that it found, or in its reverse if backward, as the engine draws it for
// the statement. It reads nothing, and does not look at the context.
type returned struct {
	data     [][]*s4qlpb.Value
	cols     []Column
	backward bool
}

// newReturned rows of the RETURNING r, in the order of the reading how.
func newReturned(r *returning, how reading) *returned {
	data := r.rows
	if how.bugs.dropLast && len(data) > 1 {
		// The planted bug: the last row is left out.
		data = data[:len(data)-1]
	}
	if how.backward {
		data = slices.Clone(data)
		slices.Reverse(data)
	}
	return &returned{data: data, cols: r.columns(), backward: how.backward}
}

func (r *returned) columns() []Column {
	return r.cols
}

// scanned is nil: the rows read no table.
func (r *returned) scanned() *scan {
	return nil
}

// rows of the statement, which never fail: the change is made, and the rows hold nothing, so the
// end of the context does not end them, unlike the rows of a query.
func (r *returned) rows(context.Context) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		for _, row := range r.data {
			if !yield(row, nil) {
				return
			}
		}
	}
}
