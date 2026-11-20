package engine

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"iter"
	"math"
	"slices"
	"strings"

	"maragu.dev/s4ql/internal/core"
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
// They build only the plans that a statement of [stmt.Check] gives. A plan of another shape, such as
// an insert of several rows or into a table whose key is several columns, fails with an internal
// error, because what SQLite gives for it has not been compared with SQLite.

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
	// scanned is the scan that reads the rows of the table that the rows of the source come from.
	scanned() *scan
}

// sink of a statement that changes the database.
type sink interface {
	// check what the statement would write, before it writes anything: "not supported" for what
	// s4ql does not support, where SQLite would run the statement or fail it while it steps it.
	check(ctx context.Context) error
	// run the statement, and give the number of rows that it changed.
	run(ctx context.Context) (int64, error)
}

// scan gives the rows of a table as its access reads them: every row in the order of the primary
// key, or the rows that the access finds by the primary key or by an index, in the order of its key,
// or in the reverse of either. It ends with the error of the context when the context ends.
type scan struct {
	r     reader
	table *s4qlpb.CreateTable
	// access is how the scan reads the rows of the table.
	access access
}

func (s *scan) scanned() *scan {
	return s
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
		name, a := s.table.GetTable(), s.access
		switch {
		case a.none:
			return
		case a.point != nil:
			row, err := s.r.get(ctx, name, a.point)
			if err != nil {
				yield(nil, err)
			} else if row != nil {
				yield(row, nil)
			}
			return
		}
		rows := s.r.scan(ctx, name, a.scan())
		for row, err := range rows {
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

func (p *project) scanned() *scan {
	return p.src.scanned()
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

// filter gives the rows of its source for which every one of its conditions is true, in the order
// of the source.
type filter struct {
	src        source
	conditions []condition
}

// condition of a [filter] on a row: the value at position compared with value by op, or tested for
// NULL, with a value of nil.
type condition struct {
	position int
	op       stmt.Operator
	value    *s4qlpb.Value
}

func (f *filter) columns() []Column {
	return f.src.columns()
}

func (f *filter) scanned() *scan {
	return f.src.scanned()
}

func (f *filter) rows(ctx context.Context) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		for row, err := range f.src.rows(ctx) {
			if err != nil {
				yield(nil, err)
				return
			}
			ok, err := f.holds(row)
			if err != nil {
				yield(nil, err)
				return
			}
			if ok && !yield(row, nil) {
				return
			}
		}
	}
}

// holds reports whether every condition is true of the row.
func (f *filter) holds(row []*s4qlpb.Value) (bool, error) {
	for _, c := range f.conditions {
		ok, err := c.holds(row)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// holds reports whether the condition is true of the row. A comparison with NULL on either side is
// neither true nor false, as in SQLite, and gives false, as a WHERE leaves the row out for both. A
// test for NULL is always true or false.
func (c condition) holds(row []*s4qlpb.Value) (bool, error) {
	if c.position < 0 || c.position >= len(row) {
		return false, internalError("a condition of the column at %v of a row of %v values", c.position, len(row))
	}
	v := row[c.position]
	switch c.op {
	case stmt.OpIsNull:
		return v.HasNull(), nil
	case stmt.OpIsNotNull:
		return !v.HasNull(), nil
	}
	if v.HasNull() || c.value.HasNull() {
		return false, nil
	}
	n, err := compare(v, c.value)
	if err != nil {
		return false, err
	}
	switch c.op {
	case stmt.OpEqual:
		return n == 0, nil
	case stmt.OpNotEqual:
		return n != 0, nil
	case stmt.OpLess:
		return n < 0, nil
	case stmt.OpLessOrEqual:
		return n <= 0, nil
	case stmt.OpGreater:
		return n > 0, nil
	case stmt.OpGreaterOrEqual:
		return n >= 0, nil
	}
	return false, internalError("a condition with the operator %v", int(c.op))
}

// compare the values a and b, which are of one type and not NULL, as SQLite compares them: integers
// and reals by their number, with 0.0 the same as -0.0, and text and blobs by their bytes, as
// memcmp compares them, with the shorter first where it begins the other, as the collation BINARY
// compares text. It gives a negative number if a is less than b, 0 if they are equal, and a positive
// number otherwise.
func compare(a, b *s4qlpb.Value) (int, error) {
	if a.WhichKind() != b.WhichKind() {
		return 0, internalError("a comparison of a value of kind %v with one of kind %v", a.WhichKind(), b.WhichKind())
	}
	switch a.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return cmp.Compare(a.GetInteger(), b.GetInteger()), nil
	case s4qlpb.Value_Real_case:
		return cmp.Compare(a.GetReal(), b.GetReal()), nil
	case s4qlpb.Value_Text_case:
		return strings.Compare(a.GetText(), b.GetText()), nil
	case s4qlpb.Value_Blob_case:
		return bytes.Compare(a.GetBlob(), b.GetBlob()), nil
	}
	return 0, internalError("a comparison of values of kind %v", a.WhichKind())
}

// insert writes each row of its source into a table whose primary key is one column, as a new row,
// and fails with SQLite's error for a row whose key the table has, also one that an earlier row of
// the source wrote. Each row of the source has a value for each column of the table, in the order
// of the table, and NULL for a column that the statement leaves out.
//
// Its check fails, in SQLite's order, with SQLite's error for NULL in a column that does not take
// it, the key among them, and then with "not supported" for a value of another type than its
// column.
type insert struct {
	w     writer
	table *s4qlpb.CreateTable
	// key is the position of the column of the primary key of the table.
	key int
	src *values
	// exprs are the expressions that the values of each row of the source come from, in the order of
	// the table, with nil for a column that the statement leaves out, for the messages of the errors
	// of the check.
	exprs [][]Expr
	// skipUnique is the planted bug that writes a row without the check of its key, so that it
	// replaces the row with its key.
	skipUnique bool
	// convertReal is the planted bug that stores a real that is a whole number in an INT or INTEGER
	// column as the integer.
	convertReal bool
}

// check each row as SQLite's constraint checks do: first the NOT NULL of each column, in the order
// of the table, the key among them, and then the type of each column, in the order of the table,
// where SQLite has its OP_TypeCheck, which comes before the check of the key. A NULL that the
// statement gives and one of a column that it leaves out are the same to both checks.
func (i *insert) check(context.Context) error {
	if len(i.src.data) != len(i.exprs) {
		return internalError("an insert of %v rows from %v rows of expressions", len(i.src.data), len(i.exprs))
	}
	columns := i.table.GetColumns()
	for n, row := range i.src.data {
		if len(row) != len(columns) || len(i.exprs[n]) != len(columns) {
			return internalError("an insert of %v values from %v expressions into %v columns", len(row), len(i.exprs[n]), len(columns))
		}
		for position, v := range row {
			if v == nil {
				return internalError("an insert without a value for the column %v", columns[position].GetName())
			}
			if v.HasNull() && (position == i.key || !columns[position].GetNullable()) {
				return notNullFailed(i.table.GetTable(), columns[position].GetName())
			}
		}
		for position, v := range row {
			typ := columns[position].GetType()
			if f := v.GetReal(); i.convertReal && v.HasReal() && (typ == s4qlpb.Type_TYPE_INT || typ == s4qlpb.Type_TYPE_INTEGER) &&
				f == math.Trunc(f) && math.Abs(f) < 1<<63 {
				// The planted bug: the real becomes the integer, as SQLite converts it.
				v = s4qlpb.Value_builder{Integer: new(int64(f))}.Build()
				row[position] = v
			}
			if err := checkType(i.table, position, v, i.exprs[n][position], "for"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (i *insert) run(ctx context.Context) (int64, error) {
	name, key := i.table.GetTable(), i.key
	var n int64
	for row, err := range i.src.rows(ctx) {
		if err != nil {
			return n, err
		}
		if i.skipUnique {
			// The planted bug: the row replaces the row with its key.
			if err := i.w.upsert(ctx, name, row); err != nil {
				return n, err
			}
			n++
			continue
		}
		// SQLite builds the record of the row before it looks for its key, so a row whose record is
		// too long gets `string or blob too big` before `UNIQUE constraint failed`. The storage
		// measures the row as SQLite does, and inserts nothing for a key that the table has, also one
		// that an earlier row of the source wrote.
		inserted, err := i.w.insertNew(ctx, name, row)
		if err != nil {
			return n, err
		}
		if !inserted {
			return n, uniqueFailed(name, i.table.GetColumns()[key].GetName())
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

func (c *createTable) check(context.Context) error { return nil }

func (c *createTable) run(ctx context.Context) (int64, error) {
	return 0, c.w.createTable(ctx, c.ct)
}

// dropTable drops a table.
type dropTable struct {
	w    writer
	name string
}

func (d *dropTable) check(context.Context) error { return nil }

func (d *dropTable) run(ctx context.Context) (int64, error) {
	return 0, d.w.dropTable(ctx, d.name)
}

// createIndex creates an index.
type createIndex struct {
	w  writer
	ci *s4qlpb.CreateIndex
}

func (c *createIndex) check(context.Context) error { return nil }

func (c *createIndex) run(ctx context.Context) (int64, error) {
	return 0, c.w.createIndex(ctx, c.ci)
}

// dropIndex drops an index.
type dropIndex struct {
	w    writer
	name string
}

func (d *dropIndex) check(context.Context) error { return nil }

func (d *dropIndex) run(ctx context.Context) (int64, error) {
	return 0, d.w.dropIndex(ctx, d.name)
}

// nothing is the sink of a statement of IF NOT EXISTS or IF EXISTS that does nothing, because its
// object is there, or not there: it writes nothing, and the statement is neither annotated nor
// committed, so that it leaves nothing in the log.
type nothing struct{}

func (nothing) check(context.Context) error { return nil }

func (nothing) run(context.Context) (int64, error) { return 0, nil }

// replaceTable is the planted bug replaceExisting: it drops the table of a create table with IF NOT
// EXISTS that exists, and creates the table of the statement in its place.
type replaceTable struct {
	drop   dropTable
	create createTable
}

func (r *replaceTable) check(context.Context) error { return nil }

func (r *replaceTable) run(ctx context.Context) (int64, error) {
	if _, err := r.drop.run(ctx); err != nil {
		return 0, err
	}
	return r.create.run(ctx)
}

// update sets columns of each row of its source: the rows that the statement finds, by a scan of a
// table whose primary key is one column, below a filter if the statement has a WHERE. It changes them
// as [changeRows] says, in the order of the key, and fails, as SQLite fails the row that it changes,
// with `string or blob too big` for a row whose record is too long, and with SQLite's error of a key
// that exists for a row whose new key another row has, also one that an earlier row of the statement
// wrote.
type update struct {
	w     writer
	table *s4qlpb.CreateTable
	// key is the position of the column of the primary key of the table.
	key int
	src source
	// positions of the columns that the statement sets, in the order of the text, and the value of
	// each, and the expression that it comes from, for the messages of the errors of the check.
	positions []int
	values    []*s4qlpb.Value
	exprs     []Expr
	// bugs planted in the update, for the tests of this package.
	bugs bugs
}

// check the values that the update sets as SQLite's constraint checks do, and as [insert.check]
// checks a row: first the NOT NULL of each column that the update sets, in the order of the table,
// the key among them, and then the type of each, in the order of the table. SQLite checks NOT NULL
// on each row that it changes, so NULL for a column that does not take it fails only an update that
// finds a row, as check reads it from the source. The type is s4ql's rule of the text, which holds
// whether the update finds a row or not.
func (u *update) check(ctx context.Context) error {
	columns := u.table.GetColumns()
	order := make([]int, len(u.positions))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return cmp.Compare(u.positions[a], u.positions[b]) })
	for _, i := range order {
		position := u.positions[i]
		if !u.values[i].HasNull() || position != u.key && columns[position].GetNullable() {
			continue
		}
		found, err := finds(ctx, u.src)
		if err != nil {
			return err
		}
		if found {
			return notNullFailed(u.table.GetTable(), columns[position].GetName())
		}
		break
	}
	for _, i := range order {
		if err := checkType(u.table, u.positions[i], u.values[i], u.exprs[i], "for"); err != nil {
			return err
		}
	}
	return nil
}

func (u *update) run(ctx context.Context) (int64, error) {
	name := u.table.GetTable()
	return changeRows(ctx, u.w, name, u.key, u.src, u.bugs, func(key []*s4qlpb.Value) error {
		old, err := u.w.get(ctx, name, key)
		if err != nil {
			return err
		}
		if old == nil {
			return internalError("the row of a key that an update found is gone")
		}
		row := slices.Clone(old)
		for i, position := range u.positions {
			row[position] = u.values[i]
		}
		same, err := compare(row[u.key], old[u.key])
		if err != nil {
			return err
		}
		if same == 0 {
			return u.w.upsert(ctx, name, row)
		}
		// A row under a new key is the old row deleted and the new one inserted, which the statement's
		// savepoint takes back if it fails. The storage measures the record of the new row before it
		// looks for its key, as SQLite does.
		if !u.bugs.keepOldKey {
			if _, err := u.w.delete(ctx, name, key); err != nil {
				return err
			}
		}
		// Else the planted bug: the row of the old key stays.
		inserted, err := u.w.insertNew(ctx, name, row)
		if err != nil {
			return err
		}
		if !inserted {
			return uniqueFailed(name, u.table.GetColumns()[u.key].GetName())
		}
		return nil
	})
}

// remove deletes each row of its source, the rows that the statement finds, as for an [update], as
// [changeRows] says.
type remove struct {
	w     writer
	table *s4qlpb.CreateTable
	// key is the position of the column of the primary key of the table.
	key int
	src source
	// bugs planted in the delete, for the tests of this package.
	bugs bugs
}

func (r *remove) check(context.Context) error { return nil }

func (r *remove) run(ctx context.Context) (int64, error) {
	name := r.table.GetTable()
	return changeRows(ctx, r.w, name, r.key, r.src, r.bugs, func(key []*s4qlpb.Value) error {
		found, err := r.w.delete(ctx, name, key)
		if err == nil && !found {
			return internalError("the row of a key that a delete found is gone")
		}
		return err
	})
}

// finds reports whether src gives a row, which it reads no further than the first.
func finds(ctx context.Context, src source) (bool, error) {
	for _, err := range src.rows(ctx) {
		return err == nil, err
	}
	return false, nil
}

// changeRows calls change with the primary key of each row of src, the rows that a statement finds
// in the table with the name, whose key is the column at the position key, in the order of the key,
// and gives the number of rows. It adds the key of each row that src gives to a [keySet] first, and
// then calls change for each key of the set. So no cursor is open on the table while change changes
// it, which SQLite leaves undefined, and a row that change writes, also under a new key, is not in
// the set: each row that src gives is changed once, as SQLite changes each row that it finds once,
// also when change writes the columns that the conditions of src read. The set is in the storage,
// so a statement changes any number of rows without holding them in memory. The bugs b are those
// planted in the statement.
func changeRows(ctx context.Context, w writer, name string, key int, src source, b bugs, change func(key []*s4qlpb.Value) error) (int64, error) {
	if b.changeNothing {
		// The planted bug: the statement finds no row.
		return 0, nil
	}
	var n int64
	err := w.keys(ctx, name, func(k keySet) error {
		for row, err := range src.rows(ctx) {
			if err != nil {
				return err
			}
			if err := k.add(ctx, []*s4qlpb.Value{row[key]}); err != nil {
				return err
			}
		}
		for key, err := range k.all(ctx) {
			if err == nil {
				err = ctx.Err()
			}
			if err != nil {
				return err
			}
			if err := change(key); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// buildQuery: the source of the rows of the plan q, in the transaction of r, with args for the
// parameters of q, and with limit for the length of a text or a blob. It fails with SQLite's errors
// in SQLite's order: for a table that does not exist, for a column of the select list that the table
// does not have, for a column of a condition that it does not have, and for a value of a condition
// that is longer than limit, as SQLite gives it when it binds an argument; and then with "not
// supported" for the first value of a condition, in the order of the text, that is of another type
// than its column. Its scan reads the rows as [build.access] says, by the reading how.
func buildQuery(ctx context.Context, r reader, q *Query, args []*s4qlpb.Value, limit int, how reading) (source, error) {
	return build{r: r, args: args, limit: limit, how: how}.node(ctx, q.Root)
}

// build of the operators of a query, in the transaction of r, as [buildQuery] says.
type build struct {
	r     reader
	args  []*s4qlpb.Value
	limit int
	how   reading
	// bugs planted in an update and a delete, for the tests of this package.
	bugs bugs
}

// node: the source of the rows of the node n of a query.
func (b build) node(ctx context.Context, n Node) (source, error) {
	switch n := n.(type) {
	case *Scan:
		t, err := b.r.table(ctx, n.Table.Value)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, noSuchTable(n.Table.Value)
		}
		return &scan{r: b.r, table: t, access: access{backward: b.how.backward}}, nil
	case *Filter:
		src, err := b.node(ctx, n.Source)
		if err != nil {
			return nil, err
		}
		return b.filter(ctx, n, src)
	case *Project:
		// SQLite resolves the names of the select list before those of the WHERE, so the filter below
		// the projection is built after it.
		inner := n.Source
		f, filtered := inner.(*Filter)
		if filtered {
			inner = f.Source
		}
		src, err := b.node(ctx, inner)
		if err != nil {
			return nil, err
		}
		p := &project{}
		columns := src.columns()
		switch {
		case n.Star:
			for i := range columns {
				p.positions = append(p.positions, i)
			}
		case len(n.Columns) == 0:
			return nil, internalError("a projection of no columns")
		default:
			// SQLite resolves the names of the select list in their order, once it has found the table.
			var names []string
			for _, c := range columns {
				names = append(names, c.Name)
			}
			of := positionsOf(names)
			for _, name := range n.Columns {
				position, ok := of[name.Fold()]
				if !ok {
					return nil, noSuchResultColumn(name)
				}
				p.positions = append(p.positions, position)
			}
		}
		if filtered {
			if src, err = b.filter(ctx, f, src); err != nil {
				return nil, err
			}
		}
		p.src = src
		return p, nil
	}
	return nil, internalError("a query of a node of type %T", n)
}

// filter of the rows of src, a scan, by the conditions of f: the names of [build.conditions], and
// then the values of [build.bind].
func (b build) filter(ctx context.Context, f *Filter, src source) (source, error) {
	s, conditions, err := b.conditions(f, src)
	if err != nil {
		return nil, err
	}
	return b.bind(ctx, f, s, conditions)
}

// conditions of the filter f of the rows of src, a scan, with the position of the column of each, and
// no value yet. It looks the columns of the conditions up in the order of the text, as SQLite does:
// the first comparison of a column that the table does not have fails with `no such column`. A test
// for NULL of such a column does not stop SQLite, which goes on to the next condition with the error
// of that column: a later test for NULL of a column that the table does not have replaces the error,
// and the next comparison ends the walk with it, before SQLite looks its own column up.
func (b build) conditions(f *Filter, src source) (*scan, []condition, error) {
	s, ok := src.(*scan)
	if !ok {
		return nil, nil, internalError("a filter of a node of type %T", src)
	}
	var names []string
	for _, c := range s.table.GetColumns() {
		names = append(names, c.GetName())
	}
	of := positionsOf(names)
	conditions := make([]condition, len(f.Conditions))
	// missing is the column of a test for NULL that the table does not have, of the last such test
	// since the last comparison, or nil.
	var missing *stmt.Name
	for i, c := range f.Conditions {
		position, ok := of[c.Column.Fold()]
		switch c.Operator {
		case stmt.OpIsNull, stmt.OpIsNotNull:
			if c.Value != nil {
				return nil, nil, internalError("a test for NULL with a value")
			}
			if !ok {
				missing = &f.Conditions[i].Column
			}
		case stmt.OpEqual, stmt.OpNotEqual, stmt.OpLess, stmt.OpLessOrEqual, stmt.OpGreater, stmt.OpGreaterOrEqual:
			if c.Value == nil {
				return nil, nil, internalError("a comparison without a value")
			}
			if missing != nil {
				return nil, nil, noSuchResultColumn(*missing)
			}
			if !ok {
				return nil, nil, noSuchResultColumn(c.Column)
			}
		default:
			return nil, nil, internalError("a condition with the operator %v", int(c.Operator))
		}
		conditions[i] = condition{position: position, op: c.Operator}
	}
	if missing != nil {
		return nil, nil, noSuchResultColumn(*missing)
	}
	return s, conditions, nil
}

// bind the values of the conditions of the filter f to conditions, which [build.conditions] gave for
// the scan s, and give the filter of the rows of s. It evaluates each value, and gives SQLite's error
// for one that is longer than the limit, and then "not supported" for the first value, in the order
// of the text, that is of another type than its column. Last it gives the scan its access, as
// [build.access] says, so the errors do not depend on the access.
func (b build) bind(ctx context.Context, f *Filter, s *scan, conditions []condition) (source, error) {
	for i, c := range f.Conditions {
		if c.Value == nil {
			continue
		}
		v, err := eval(c.Value, b.args)
		if err != nil {
			return nil, err
		}
		if err := checkLength(v, b.limit); err != nil {
			return nil, err
		}
		conditions[i].value = v
	}
	for i, c := range conditions {
		if c.value == nil {
			continue
		}
		if err := checkType(s.table, c.position, c.value, f.Conditions[i].Value, "compared with"); err != nil {
			return nil, err
		}
	}
	a, err := b.access(ctx, s.table, conditions)
	if err != nil {
		return nil, err
	}
	a.backward = b.how.backward
	s.access = a
	return &filter{src: s, conditions: conditions}, nil
}

// access to the rows of the table t for a filter of the conditions: the one that [chooseAccess]
// gives, or the one of the path that the tests force, as [forcedAccess] gives it. It reads the
// indexes of the table only if the access depends on them, so a statement that the rules give no
// row, or the row of a primary key, reads none.
func (b build) access(ctx context.Context, t *s4qlpb.CreateTable, conditions []condition) (access, error) {
	p := b.how.path
	switch p.kind {
	case pathFull:
		return access{}, nil
	case pathPrimary:
		a, _, err := forcedAccess(t, nil, conditions, p, b.how.bugs)
		return a, err
	case pathRules:
		if a, err := chooseAccess(t, nil, conditions, b.how.bugs); err != nil || a.none || a.point != nil {
			return a, err
		}
	}
	indexes, err := b.r.indexes(ctx, t.GetTable())
	if err != nil {
		return access{}, err
	}
	if p.kind == pathIndex {
		if a, ok, err := forcedAccess(t, indexes, conditions, p, b.how.bugs); err != nil || ok {
			return a, err
		}
	}
	return chooseAccess(t, indexes, conditions, b.how.bugs)
}

// buildValues: the source of the rows of v, with args for their parameters, and the expressions of
// each row. The value at the index i of a row of v is for the column at positions[i] of a table of
// width columns, and each row that it gives has a value for each column of the table, in the order
// of the table, with NULL for a column that positions do not name, as SQLite gives a column that an
// insert leaves out, whose default is NULL. Its expression is nil.
func buildValues(v *Values, args []*s4qlpb.Value, positions []int, width int) (*values, [][]Expr, error) {
	src := &values{}
	var exprs [][]Expr
	for _, row := range v.Rows {
		if len(row) != len(positions) {
			return nil, nil, internalError("a row of %v values for %v columns", len(row), len(positions))
		}
		data, es := make([]*s4qlpb.Value, width), make([]Expr, width)
		for i, e := range row {
			value, err := eval(e, args)
			if err != nil {
				return nil, nil, err
			}
			data[positions[i]], es[positions[i]] = value, e
		}
		for position, value := range data {
			if value == nil {
				data[position] = null()
			}
		}
		src.data, exprs = append(src.data, data), append(exprs, es)
	}
	return src, exprs, nil
}

// eval the constant expression e, with args for its parameters, which the engine has counted.
func eval(e Expr, args []*s4qlpb.Value) (*s4qlpb.Value, error) {
	switch e := e.(type) {
	case *Integer:
		return s4qlpb.Value_builder{Integer: new(e.Value)}.Build(), nil
	case *Real:
		return s4qlpb.Value_builder{Real: new(e.Value)}.Build(), nil
	case *Text:
		return s4qlpb.Value_builder{Text: new(e.Value)}.Build(), nil
	case *Blob:
		if e.Value == nil {
			return nil, internalError("a blob literal of no bytes that is nil")
		}
		return s4qlpb.Value_builder{Blob: e.Value}.Build(), nil
	case *Null:
		return null(), nil
	case *Parameter:
		if e.Number < 1 || e.Number > len(args) {
			return nil, internalError("the parameter %v of a statement with %v arguments", e.Number, len(args))
		}
		return args[e.Number-1], nil
	}
	panic(fmt.Sprintf("engine: an expression of type %T", e))
}

// null is the value NULL.
func null() *s4qlpb.Value {
	return s4qlpb.Value_builder{Null: &s4qlpb.Null{}}.Build()
}

// buildSink: the sink of the plan p, in the transaction of w, with args for the parameters of p, and
// with the planted bugs of b. It makes the checks that SQLite makes when it prepares the statement,
// in SQLite's order, and fails with SQLite's error: for CREATE TABLE, a table of the name, and then
// a column named twice or more columns than SQLite allows, as [stmt.CreateTable.ColumnsError] says,
// and then a table that SQLite cannot write into its schema under limit, as [schemaTooBig] says;
// for DROP TABLE, no table of the name. With IF NOT EXISTS and a table of the name, or with IF
// EXISTS and none, the sink is [nothing], as SQLite does nothing there, so a CREATE TABLE then
// gives none of its later errors; but an index of the name still gives its error. For INSERT, no
// table of the name, then a column of its list that the table does not have, in the order of the
// list, and then another number of values than the columns of its list, or than the columns of the
// table if it has no list. For INSERT it then
// makes the check that SQLite makes when it binds the arguments: a text or a blob that is longer
// than limit bytes gets SQLite's `string or blob too big`. It checks a literal so too, which SQLite
// meets in its text only with a limit below the length of the text: see the README. For UPDATE and
// DELETE, see [build.change]; their scan reads the rows as [build.access] says, by the reading how.
// For CREATE INDEX and DROP INDEX, see [buildCreateIndex] and [buildDropIndex].
func buildSink(ctx context.Context, w writer, p Plan, args []*s4qlpb.Value, b bugs, limit int, how reading) (sink, error) {
	switch p := p.(type) {
	case *Update:
		return build{r: w, args: args, limit: limit, how: how, bugs: b}.change(ctx, w, p.Source, p.Set, true)
	case *Delete:
		return build{r: w, args: args, limit: limit, how: how, bugs: b}.change(ctx, w, p.Source, nil, false)
	case *CreateIndex:
		return buildCreateIndex(ctx, w, p, limit)
	case *DropIndex:
		return buildDropIndex(ctx, w, p)
	case *CreateTable:
		existing, err := w.table(ctx, p.Table.Value)
		if err != nil {
			return nil, err
		}
		replace := existing != nil && p.IfNotExists && b.replaceExisting
		switch {
		case replace:
			// The planted bug: the table that exists is replaced below.
		case existing != nil && p.IfNotExists:
			// SQLite stops at the table of the name, before it reads the columns of the statement.
			return nothing{}, nil
		case existing != nil:
			return nil, tableExists(p.Table.Raw, p.Table.Offset)
		}
		index, err := w.index(ctx, p.Table.Value)
		if err != nil {
			return nil, err
		}
		if index != nil {
			return nil, indexNamed(p.Table.Value)
		}
		if err := columnsError(p); err != nil {
			return nil, err
		}
		if schemaTooBig(p.Table.Value, p.SQL, limit) {
			return nil, tooBig()
		}
		if len(p.PrimaryKey) != 1 || p.PrimaryKey[0] < 0 || p.PrimaryKey[0] >= len(p.Columns) {
			return nil, internalError("a table of %v columns with the key %v", len(p.Columns), p.PrimaryKey)
		}
		if p.Columns[p.PrimaryKey[0]].Nullable {
			return nil, internalError("a table whose key %v takes NULL", p.Columns[p.PrimaryKey[0]].Name.Value)
		}
		ct := s4qlpb.CreateTable_builder{Table: new(p.Table.Value), PrimaryKey: []uint32{uint32(p.PrimaryKey[0])}}
		for _, c := range p.Columns {
			column := s4qlpb.Column_builder{Name: new(c.Name.Value), Type: new(c.Type)}
			if c.Nullable {
				// Only when it is true, as the storage gives the table back.
				column.Nullable = new(true)
			}
			ct.Columns = append(ct.Columns, column.Build())
		}
		create := createTable{w: w, ct: ct.Build()}
		if replace {
			return &replaceTable{drop: dropTable{w: w, name: existing.GetTable()}, create: create}, nil
		}
		return &create, nil
	case *DropTable:
		t, err := w.table(ctx, p.Table.Value)
		if err != nil {
			return nil, err
		}
		switch {
		case t == nil && p.IfExists:
			// Also where an index has the name.
			return nothing{}, nil
		case t == nil:
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
		var positions []int
		if p.Columns != nil {
			var names []string
			for _, c := range t.GetColumns() {
				names = append(names, c.GetName())
			}
			of := positionsOf(names)
			for _, c := range p.Columns {
				position, ok := of[c.Fold()]
				if !ok {
					return nil, noSuchColumn(p.Table.Value, c.Value)
				}
				positions = append(positions, position)
			}
		}
		width := len(t.GetColumns())
		if key := t.GetPrimaryKey(); len(key) != 1 || int(key[0]) >= width {
			return nil, internalError("an insert into a table of %v columns with the key %v", width, key)
		}
		named := make([]bool, width)
		for _, position := range positions {
			if named[position] {
				return nil, internalError("an insert that names the column %v twice", t.GetColumns()[position].GetName())
			}
			named[position] = true
		}
		if b.listOrder {
			// The planted bug: the values go to the columns in the order of the list.
			for i := range positions {
				positions[i] = i
			}
		}
		v, ok := p.Source.(*Values)
		if !ok {
			return nil, internalError("an insert from a node of type %T", p.Source)
		}
		if len(v.Rows) != 1 {
			return nil, internalError("an insert of %v rows", len(v.Rows))
		}
		n := len(v.Rows[0])
		switch {
		case p.Columns == nil && n != width:
			return nil, valuesSupplied(p.Table.Value, width, n)
		case p.Columns == nil:
			positions = make([]int, width)
			for i := range positions {
				positions[i] = i
			}
		case n != len(p.Columns):
			return nil, valuesForColumns(n, len(p.Columns))
		}
		src, exprs, err := buildValues(v, args, positions, width)
		if err != nil {
			return nil, err
		}
		if b.zeroLeftOut && p.Columns != nil {
			// The planted bug: a column that takes NULL and that the list leaves out gets the zero
			// value of its type.
			for position, c := range t.GetColumns() {
				if !named[position] && c.GetNullable() {
					src.data[0][position] = zeroOf(c.GetType())
				}
			}
		}
		for _, row := range src.data {
			for _, v := range row {
				if err := checkLength(v, limit); err != nil {
					return nil, err
				}
			}
		}
		return &insert{w: w, table: t, key: int(t.GetPrimaryKey()[0]), src: src, exprs: exprs, skipUnique: b.skipUnique, convertReal: b.convertReal}, nil
	}
	return nil, internalError("a plan of type %T", p)
}

// change is the sink of an UPDATE, if isUpdate, with the assignments set, or of a DELETE, that
// changes the rows of source, a [*Scan], or a [*Filter] of one, in the transaction of w. It fails with
// SQLite's errors in SQLite's order: for a table that does not exist; for a column of the SET that
// the table does not have, the first in the order of the text, with the value of its name and
// nothing about quotes, as SQLite's update itself gives it; for a column of the WHERE, as
// [build.conditions] says; and for a value of the SET and then of the WHERE that is longer than
// the limit, as SQLite gives it when it binds an argument, the first in the order of the text, which
// is the order of the parameters too. Then it fails with "not supported" for a value of the WHERE of
// another type than its column, as a select does, and gives the scan its access, as
// [build.bind] says. A table without a WHERE is read as the reading says too.
func (b build) change(ctx context.Context, w writer, source Node, set []Assignment, isUpdate bool) (sink, error) {
	f, filtered := source.(*Filter)
	inner := source
	if filtered {
		inner = f.Source
	}
	if _, ok := inner.(*Scan); !ok {
		return nil, internalError("a change of the rows of a node of type %T", inner)
	}
	src, err := b.node(ctx, inner)
	if err != nil {
		return nil, err
	}
	s := src.(*scan)
	t := s.table
	key := t.GetPrimaryKey()
	if len(key) != 1 || int(key[0]) >= len(t.GetColumns()) {
		return nil, internalError("a change of a table of %v columns with the key %v", len(t.GetColumns()), key)
	}
	var names []string
	for _, c := range t.GetColumns() {
		names = append(names, c.GetName())
	}
	of := positionsOf(names)
	positions := make([]int, len(set))
	named := make([]bool, len(names))
	for i, a := range set {
		position, ok := of[a.Column.Fold()]
		if !ok {
			return nil, noSuchSetColumn(a.Column)
		}
		if named[position] {
			return nil, internalError("an update that sets the column %v twice", names[position])
		}
		positions[i], named[position] = position, true
	}
	var conditions []condition
	if filtered {
		if _, conditions, err = b.conditions(f, s); err != nil {
			return nil, err
		}
	}
	values := make([]*s4qlpb.Value, len(set))
	exprs := make([]Expr, len(set))
	for i, a := range set {
		v, err := eval(a.Value, b.args)
		if err != nil {
			return nil, err
		}
		if err := checkLength(v, b.limit); err != nil {
			return nil, err
		}
		values[i], exprs[i] = v, a.Value
	}
	if filtered {
		if src, err = b.bind(ctx, f, s, conditions); err != nil {
			return nil, err
		}
	}
	if !isUpdate {
		return &remove{w: w, table: t, key: int(key[0]), src: src, bugs: b.bugs}, nil
	}
	if len(set) == 0 {
		return nil, internalError("an update that sets no column")
	}
	return &update{w: w, table: t, key: int(key[0]), src: src, positions: positions, values: values, exprs: exprs, bugs: b.bugs}, nil
}

// buildCreateIndex is the sink of the plan p of a CREATE INDEX, in the transaction of w. It fails with
// SQLite's errors in SQLite's order: for a table that does not exist, with the name of the schema
// before the name of the table; for a table that has the name of the index, which a table and an
// index share; for an index that has it, on any table; for the first column, in the order of the
// text, that the table does not have; and for an index that SQLite cannot write into its schema
// under limit, as [indexTooBig] says. Then it fails with "not supported" for an index of more than
// [core.MaxIndexColumns] columns, which is what the storage holds where SQLite takes 2,000, at the
// first column beyond them. With IF NOT EXISTS and an index of the name, the sink is [nothing], after
// the errors of the table, as SQLite does nothing there, so none of the later errors and refusals is
// given.
func buildCreateIndex(ctx context.Context, w writer, p *CreateIndex, limit int) (sink, error) {
	t, err := w.table(ctx, p.Table.Value)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, noSuchTable("main." + p.Table.Value)
	}
	named, err := w.table(ctx, p.Index.Value)
	if err != nil {
		return nil, err
	}
	if named != nil {
		return nil, tableNamed(p.Index.Value)
	}
	existing, err := w.index(ctx, p.Index.Value)
	if err != nil {
		return nil, err
	}
	switch {
	case existing != nil && p.IfNotExists:
		// SQLite stops at the index of the name, on any table, before it reads the columns of the
		// statement.
		return nothing{}, nil
	case existing != nil:
		return nil, indexExists(p.Index.Value)
	}
	var names []string
	for _, c := range t.GetColumns() {
		names = append(names, c.GetName())
	}
	of := positionsOf(names)
	ci := s4qlpb.CreateIndex_builder{Index: new(p.Index.Value), Table: new(t.GetTable())}
	held := make([]bool, len(names))
	for _, c := range p.Columns {
		position, ok := of[c.Name.Fold()]
		if !ok {
			return nil, noSuchResultColumn(c.Name)
		}
		if held[position] {
			return nil, internalError("an index that names the column %v twice", names[position])
		}
		held[position] = true
		column := s4qlpb.IndexColumn_builder{Column: new(uint32(position))}
		if c.Descending {
			// Only when it is true, as the storage gives the index back.
			column.Descending = new(true)
		}
		ci.Columns = append(ci.Columns, column.Build())
	}
	if indexTooBig(p.Index.Value, t.GetTable(), p.SQL, limit) {
		return nil, tooBig()
	}
	if len(p.Columns) > core.MaxIndexColumns {
		return nil, &stmt.NotSupportedError{What: fmt.Sprint("index of more than ", core.MaxIndexColumns, " columns"), Offset: p.Columns[core.MaxIndexColumns].Name.Offset}
	}
	return &createIndex{w: w, ci: ci.Build()}, nil
}

// buildDropIndex is the sink of the plan p of a DROP INDEX, in the transaction of w. It fails with
// SQLite's error for no index of the name, also when a table has the name, or, with IF EXISTS, is
// [nothing] there.
func buildDropIndex(ctx context.Context, w writer, p *DropIndex) (sink, error) {
	ix, err := w.index(ctx, p.Index.Value)
	if err != nil {
		return nil, err
	}
	switch {
	case ix == nil && p.IfExists:
		// Also where a table has the name.
		return nothing{}, nil
	case ix == nil:
		return nil, noSuchIndex(p.Index.Value)
	}
	return &dropIndex{w: w, name: ix.GetIndex()}, nil
}

// columnsError of the plan p of a table that does not exist: SQLite's error for a column named twice
// or for more columns than SQLite allows, as [stmt.CreateTable.ColumnsError] gives it, or nil.
func columnsError(p *CreateTable) error {
	s := &stmt.CreateTable{Table: p.Table}
	for _, c := range p.Columns {
		s.Columns = append(s.Columns, stmt.Column{Name: c.Name})
	}
	if err := s.ColumnsError(); err != nil {
		return err
	}
	return nil
}

// positionsOf the columns of the names, by the fold of each name, as SQLite compares names: the
// position of the first column of each fold.
func positionsOf(names []string) map[string]int {
	positions := make(map[string]int, len(names))
	for i, name := range names {
		if _, ok := positions[fold(name)]; !ok {
			positions[fold(name)] = i
		}
	}
	return positions
}

// fold the ASCII letters of a name to lower case, as [stmt.Name.Fold] does.
func fold(name string) string {
	return stmt.Name{Value: name}.Fold()
}
