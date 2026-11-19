package engine

import (
	"context"
	"fmt"
	"iter"
	"slices"

	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// The operators that execute a plan. A [source] hands its rows to the operator above it, one at a
// time, through a push-style iterator: the operator above ranges over the rows, and ends the source
// by breaking out of the loop. A [sink] runs a statement that changes the database, and reports the
// number of rows that it changed.
//
// Operators are built from a plan by [buildQuery] and [buildSink], in the transaction that runs them,
// and read and change the database only through the transaction's [reader] or [writer].
//
// They build only the plans that a statement of slice 1 gives. A plan of another shape, such as an
// insert of several values or a projection without `*`, fails with an internal error, because what
// SQLite gives for it has not been compared with SQLite.

// Column of the rows that an operator gives: its name, and its declared type, which is empty for a
// column that is not a column of a table.
type Column struct {
	Name     string
	DeclType string
}

// rowSource of rows, which have no names of columns.
type rowSource interface {
	// rows, which end at the first error.
	rows(ctx context.Context) iter.Seq2[[]*s4qlpb.Value, error]
}

// source of rows with named columns.
type source interface {
	rowSource
	// columns of each row.
	columns() []Column
}

// sink of a statement that changes the database.
type sink interface {
	// run the statement, and give the number of rows that it changed.
	run(ctx context.Context) (int64, error)
}

// scan gives the rows of a table in the order of its primary key. It ends with the error of the
// context when the context ends.
type scan struct {
	r     reader
	table *s4qlpb.CreateTable
}

func (s *scan) columns() []Column {
	cs := make([]Column, len(s.table.GetColumns()))
	for i, c := range s.table.GetColumns() {
		cs[i] = Column{Name: c.GetName(), DeclType: typeName(c.GetType())}
	}
	return cs
}

func (s *scan) rows(ctx context.Context) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		for row, err := range s.r.scan(ctx, s.table.GetTable()) {
			if err == nil {
				err = ctx.Err()
			}
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(row, nil) {
				return
			}
		}
	}
}

// values gives rows of constants.
type values struct {
	data [][]*s4qlpb.Value
}

func (v *values) rows(ctx context.Context) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		for _, row := range v.data {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			if !yield(row, nil) {
				return
			}
		}
	}
}

// project gives the columns of each row of its source at the positions, in their order: none if
// it has no positions.
type project struct {
	src       source
	positions []int
}

func (p *project) columns() []Column {
	all := p.src.columns()
	cs := make([]Column, len(p.positions))
	for i, position := range p.positions {
		cs[i] = all[position]
	}
	return cs
}

func (p *project) rows(ctx context.Context) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		for row, err := range p.src.rows(ctx) {
			if err != nil {
				yield(nil, err)
				return
			}
			out := make([]*s4qlpb.Value, len(p.positions))
			for i, position := range p.positions {
				out[i] = row[position]
			}
			if !yield(out, nil) {
				return
			}
		}
	}
}

// insert writes each row of its source into a table of one column, which is its primary key, as a
// new row, and fails with SQLite's error for a row whose key the table has, also one that an earlier
// row of the source wrote.
type insert struct {
	w     writer
	table *s4qlpb.CreateTable
	src   rowSource
	// skipUnique is the planted bug that writes a row without the check of its key.
	skipUnique bool
}

func (i *insert) run(ctx context.Context) (int64, error) {
	name := i.table.GetTable()
	var n int64
	for row, err := range i.src.rows(ctx) {
		if err != nil {
			return n, err
		}
		if !i.skipUnique {
			existing, err := i.w.get(ctx, name, row)
			if err != nil {
				return n, err
			}
			if existing != nil {
				return n, uniqueFailed(name, i.table.GetColumns()[0].GetName())
			}
		}
		if err := i.w.upsert(ctx, name, row); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// createTable creates a table.
type createTable struct {
	w  writer
	ct *s4qlpb.CreateTable
}

func (c *createTable) run(ctx context.Context) (int64, error) {
	return 0, c.w.createTable(ctx, c.ct)
}

// dropTable drops a table.
type dropTable struct {
	w    writer
	name string
}

func (d *dropTable) run(ctx context.Context) (int64, error) {
	return 0, d.w.dropTable(ctx, d.name)
}

// buildQuery: the source of the rows of the plan q, in the transaction of r. It fails with SQLite's
// error for a table that does not exist.
func buildQuery(ctx context.Context, r reader, q *Query) (source, error) {
	return buildNode(ctx, r, q.Root)
}

// buildNode: the source of the rows of the node n of a query.
func buildNode(ctx context.Context, r reader, n Node) (source, error) {
	switch n := n.(type) {
	case *Scan:
		t, err := r.table(ctx, n.Table.Value)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, noSuchTable(n.Table.Value)
		}
		return &scan{r: r, table: t}, nil
	case *Project:
		if !n.Star {
			return nil, internalError("a projection without *")
		}
		src, err := buildNode(ctx, r, n.Source)
		if err != nil {
			return nil, err
		}
		p := &project{src: src}
		for i := range src.columns() {
			p.positions = append(p.positions, i)
		}
		return p, nil
	}
	return nil, internalError("a query of a node of type %T", n)
}

// buildValues: the source of the rows of the node n of an insert, which is one row of one value, with
// args for its parameters.
func buildValues(n Node, args []*s4qlpb.Value) (rowSource, error) {
	v, ok := n.(*Values)
	if !ok {
		return nil, internalError("an insert from a node of type %T", n)
	}
	if len(v.Rows) != 1 {
		return nil, internalError("an insert of %v rows", len(v.Rows))
	}
	if len(v.Rows[0]) != 1 {
		return nil, internalError("an insert of %v values", len(v.Rows[0]))
	}
	value, err := eval(v.Rows[0][0], args)
	if err != nil {
		return nil, err
	}
	return &values{data: [][]*s4qlpb.Value{{value}}}, nil
}

// eval the constant expression e, with args for its parameters, which the engine has counted.
func eval(e Expr, args []*s4qlpb.Value) (*s4qlpb.Value, error) {
	switch e := e.(type) {
	case *Integer:
		return s4qlpb.Value_builder{Integer: new(e.Value)}.Build(), nil
	case *Parameter:
		if e.Number < 1 || e.Number > len(args) {
			return nil, internalError("the parameter %v of a statement with %v arguments", e.Number, len(args))
		}
		return args[e.Number-1], nil
	}
	panic(fmt.Sprintf("engine: an expression of type %T", e))
}

// buildSink: the sink of the plan p, in the transaction of w, with args for the parameters of p, and
// with the planted bugs of b. It makes the
// checks that SQLite makes when it prepares the statement, in SQLite's order, and fails with SQLite's
// error: for CREATE TABLE, a table of the name; for DROP TABLE, no table of the name; for INSERT, no
// table of the name, and then a column that the table does not have.
func buildSink(ctx context.Context, w writer, p Plan, args []*s4qlpb.Value, b bugs) (sink, error) {
	switch p := p.(type) {
	case *CreateTable:
		existing, err := w.table(ctx, p.Table.Value)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return nil, tableExists(p.Table.Raw, p.Table.Offset)
		}
		if len(p.Columns) != 1 || !slices.Equal(p.PrimaryKey, []int{0}) {
			return nil, internalError("a table of %v columns with the key %v", len(p.Columns), p.PrimaryKey)
		}
		c := p.Columns[0]
		ct := s4qlpb.CreateTable_builder{
			Table:      new(p.Table.Value),
			Columns:    []*s4qlpb.Column{s4qlpb.Column_builder{Name: new(c.Name.Value), Type: new(c.Type)}.Build()},
			PrimaryKey: []uint32{0},
		}
		return &createTable{w: w, ct: ct.Build()}, nil
	case *DropTable:
		t, err := w.table(ctx, p.Table.Value)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, noSuchTable(p.Table.Value)
		}
		return &dropTable{w: w, name: t.GetTable()}, nil
	case *Insert:
		t, err := w.table(ctx, p.Table.Value)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, noSuchTable(p.Table.Value)
		}
		for _, c := range p.Columns {
			if columnPosition(t, c) < 0 {
				return nil, noSuchColumn(p.Table.Value, c.Value)
			}
		}
		if len(p.Columns) != 1 || len(t.GetColumns()) != 1 || !slices.Equal(t.GetPrimaryKey(), []uint32{0}) {
			return nil, internalError("an insert of %v columns into a table of %v columns with the key %v", len(p.Columns), len(t.GetColumns()), t.GetPrimaryKey())
		}
		src, err := buildValues(p.Source, args)
		if err != nil {
			return nil, err
		}
		return &insert{w: w, table: t, src: src, skipUnique: b.skipUnique}, nil
	}
	return nil, internalError("a plan of type %T", p)
}

// columnPosition of the column with the name in the table, compared as SQLite compares names, or -1
// if the table has none.
func columnPosition(t *s4qlpb.CreateTable, name stmt.Name) int {
	for i, c := range t.GetColumns() {
		if fold(c.GetName()) == name.Fold() {
			return i
		}
	}
	return -1
}

// fold the ASCII letters of a name to lower case, as [stmt.Name.Fold] does.
func fold(name string) string {
	return stmt.Name{Value: name}.Fold()
}
