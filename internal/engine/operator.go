package engine

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"math"
	"slices"
	"strings"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/expr"
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
// or in the reverse of either, or, for an access whose rows are sorted, in the order of its ORDER
// BY. It ends with the error of the context when the context ends: it looks at the context before
// each row, and the cache stops a sort of the rows of the access, in which it reads every row
// before it gives the first, when the context ends during it.
type scan struct {
	r     reader
	table *s4qlpb.CreateTable
	// access is how the scan reads the rows of the table.
	access access
	// subqueries holds what each scalar subquery of the statement read, in the order of the text, once
	// it is computed, for the event of the statement.
	subqueries []subqueryRead
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
	// again computes the subqueries of the conditions again, and the access of the scan for their
	// values, for the planted bug ownChanges, or is nil.
	again func(ctx context.Context) error
}

// condition of a [filter] on a row: the value at position compared with value by op, or tested for
// NULL, with a value of nil.
//
// With an expression, test computes the side that the condition tests, in place of the value at
// position, and per computes the value, in place of value, for each row; a condition with either is
// computed, as [condition.computed] says, and serves no access, as [bounds] says. constant computes
// value once for each run of the statement, as [build.constants] does, before the access is chosen.
type condition struct {
	position int
	op       stmt.Operator
	value    *s4qlpb.Value
	// valueFirst reports whether the statement writes the value on the left, as [Condition] says;
	// op is the operator turned round then. It is read only by the planted bug unturnedBound and by
	// tests: op is never to be turned round again from it.
	valueFirst bool
	// test, per, and constant are the expressions of the condition, each nil where it has none.
	test, per, constant *expr.Bound
	// hoisted are the subtrees of test and per that SQLite computes before any row, as [hoisted]
	// says, in the order of the text.
	hoisted []*expr.Bound
	// lazy reports whether constant is text with a call, which SQLite computes when the first row
	// reaches it, unless it serves its access; failed is SQLite's error for it, once [build.constants]
	// found it too long there, which the condition gives on the first row that it is evaluated on.
	lazy   bool
	failed error
	// text is the expression of constant where it is text, or the zero Expression, whose subtrees
	// SQLite computes before any row of the statement also in a subquery that it does not compute, as
	// [build.hoist] says.
	text stmt.Expression
	// paired reports whether the value of the condition is the column alone at pair of the row, as in
	// `a = b`, in place of value.
	paired bool
	pair   int
	// bugs planted in the conditions of arithmetic, for the tests of this package: see [bugs].
	nullCompares, errorSkipsRow, unserved bool
}

// perRow reports whether the condition has a side that is computed for each row, or compares two
// columns of the row.
func (c condition) perRow() bool {
	// With the planted bug constantsServeNothing, unserved is set for a constant.
	return c.test != nil || c.per != nil || c.paired || c.unserved || c.failed != nil
}

// bounds of the conditions: those that may serve the access, which are those that compare a column
// with a value of one run of the statement, or test it for NULL. A condition that is computed for
// each row is left to the filter.
func bounds(conditions []condition) []condition {
	if !slices.ContainsFunc(conditions, condition.perRow) {
		return conditions
	}
	var bs []condition
	for _, c := range conditions {
		if !c.perRow() {
			bs = append(bs, c)
		}
	}
	return bs
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
	if c.failed != nil {
		return false, c.failed
	}
	if c.test != nil || c.per != nil {
		ok, err := c.computed(row)
		if err != nil && c.errorSkipsRow && errors.Is(err, expr.ErrEvaluation) {
			// The planted bug: the row is left out.
			return false, nil
		}
		return ok, err
	}
	if c.position < 0 || c.position >= len(row) {
		return false, internalError("a condition of the column at %v of a row of %v values", c.position, len(row))
	}
	v := row[c.position]
	if c.paired {
		if c.pair < 0 || c.pair >= len(row) {
			return false, internalError("a condition of the column at %v of a row of %v values", c.pair, len(row))
		}
		c.value = row[c.pair]
		if c.nullCompares && (v.HasNull() || c.value.HasNull()) && c.op != stmt.OpIsNull && c.op != stmt.OpIsNotNull {
			// The planted bug.
			return true, nil
		}
	}
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

// computed reports whether the condition, whose side that it tests or whose value is computed for
// each row, is true of the row. It computes both sides, in the order of the text, also where the
// first is NULL, so that an error of the second comes, as an operator of arithmetic computes both its
// operands; the first error ends it. A side that is NULL makes the comparison neither true nor false,
// and it gives false. The types of the two sides are one, which the checks of the statement made
// sure of.
func (c condition) computed(row []*s4qlpb.Value) (bool, error) {
	var tested, value scalar
	var err error
	if c.valueFirst {
		if value, err = c.valueOf(row); err != nil {
			return false, err
		}
	}
	if c.test != nil {
		v, err := c.test.Eval(row)
		if err != nil {
			return false, fromExpr(err)
		}
		if tested, err = scalarOf(v); err != nil {
			return false, err
		}
	}
	if !c.valueFirst {
		if value, err = c.valueOf(row); err != nil {
			return false, err
		}
	}
	if c.test == nil {
		if c.position < 0 || c.position >= len(row) {
			return false, internalError("a condition of the column at %v of a row of %v values", c.position, len(row))
		}
		if tested, err = protoScalar(row[c.position]); err != nil {
			return false, err
		}
	}
	if tested.class == classAny || value.class == classAny {
		// With the planted bug nullCompares, true.
		return c.nullCompares, nil
	}
	n, err := compareScalars(tested, value)
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
	return false, internalError("a computed condition with the operator %v", int(c.op))
}

// valueOf the condition for the row: the value of per, or the value of the condition.
func (c condition) valueOf(row []*s4qlpb.Value) (scalar, error) {
	if c.per == nil {
		return protoScalar(c.value)
	}
	v, err := c.per.Eval(row)
	if err != nil {
		return scalar{}, fromExpr(err)
	}
	return scalarOf(v)
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

// limit gives the first n rows of its source, in the order of the source, and ends the loop over the
// source with the last of them, so the source reads no row after it. With an n of 0 it gives no row,
// and does not start the source, which then reads nothing.
type limit struct {
	src source
	n   int64
}

func (l *limit) columns() []Column {
	return l.src.columns()
}

func (l *limit) scanned() *scan {
	return l.src.scanned()
}

func (l *limit) rows(ctx context.Context) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		if l.n <= 0 {
			return
		}
		var given int64
		for row, err := range l.src.rows(ctx) {
			if err != nil {
				yield(nil, err)
				return
			}
			given++
			if !yield(row, nil) || given == l.n {
				return
			}
		}
	}
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
	// ret holds the rows of its RETURNING, or is nil for an insert without one.
	ret *returning
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
			i.ret.add(row)
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
		i.ret.add(row)
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
// as [changeRows] says, in SQLite's order, as [changeOrder] says, and fails, as SQLite fails the row
// that it changes,
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
	// each, and the expression that it comes from, for the messages of the errors of the check. The
	// value is nil for a value that is computed for each row: an arithmetic expression with a column,
	// whose bound expression is in computed, and a column alone, whose position is in copied. An
	// expression without a column has its bound expression in computed and its value in values, but
	// one of text that is too long, which SQLite computes for each row and the update computes so too.
	positions []int
	values    []*s4qlpb.Value
	exprs     []Expr
	computed  []*expr.Bound
	copied    []int
	// hoisted are the subtrees of the expressions of text that SQLite computes before any row, as
	// [hoisted] says, in the order of the text.
	hoisted []*expr.Bound
	// order holds the indexes of positions in the order of the table.
	order []int
	// changes is the order in which the update changes its rows, SQLite's, as [changeOrder] says.
	changes changeOrder
	// ret holds the rows of its RETURNING, or is nil for an update without one.
	ret *returning
	// bugs planted in the update, for the tests of this package.
	bugs bugs
}

// check the values that the update sets as SQLite's constraint checks do, and as [insert.check]
// checks a row: first the NOT NULL of each column that the update sets, in the order of the table,
// the key among them, and then the type of each, in the order of the table. SQLite checks NOT NULL
// on each row that it changes, so NULL for a column that does not take it fails only an update that
// finds a row, as check reads it from the source. The type is s4ql's rule of the text, which holds
// whether the update finds a row or not.
//
// A value of an expression has the class of its expression, as [comparable] says, whatever its rows, and
// a column alone that of its column. An update with a value that is computed for each row checks the
// NOT NULL of every column that it sets on each row that it changes, in [update.run], so the
// refusal of a type comes before it there.
func (u *update) check(ctx context.Context) error {
	columns := u.table.GetColumns()
	notNull := u.order
	if u.perRow() {
		notNull = nil
	}
	for _, i := range notNull {
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
	return u.checkTypes()
}

// checkTypes of the values that the update sets, in the order of the table, as [update.check] says.
func (u *update) checkTypes() error {
	columns := u.table.GetColumns()
	for _, i := range u.order {
		position := u.positions[i]
		into := columnClass(columns[position].GetType())
		switch {
		case u.copied[i] >= 0:
			// With the planted bug copyAcross, a copy is never refused.
			if columnClass(columns[u.copied[i]].GetType()) != into && !u.bugs.copyAcross {
				return fmt.Errorf("%w: %v for %v", stmt.ErrNotSupported, columnName(u.table, u.copied[i]), columnName(u.table, position))
			}
		case u.computed[i] != nil:
			arithmetic, ok := u.exprs[i].(*Arithmetic)
			if !ok {
				return internalError("a computed value of SET of type %T", u.exprs[i])
			}
			if c := expressionClass(arithmetic.Expression, u.computed[i]); !comparable(into, c) {
				return fmt.Errorf("%w: %v for %v", stmt.ErrNotSupported, expressionName(c), columnName(u.table, position))
			}
		default:
			if err := checkType(u.table, position, u.values[i], u.exprs[i], "for"); err != nil {
				return err
			}
		}
	}
	return nil
}

// perRow reports whether a value of the update is computed for each row.
func (u *update) perRow() bool {
	for _, v := range u.values {
		if v == nil {
			return true
		}
	}
	return false
}

// newRow of the update from the row old, as it was before the statement: each value that it sets,
// in the order of the text, which computes those of each row from old; and then, if a value is
// computed for each row, SQLite's error for the first column, in the order of the table, that gets
// NULL and does not take it.
func (u *update) newRow(old []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	row := slices.Clone(old)
	perRow := false
	if u.bugs.setFromNew {
		// The planted bug: each value from the row as the assignments before it left it.
		old = row
	}
	for i, position := range u.positions {
		switch {
		case u.values[i] != nil:
			row[position] = u.values[i]
		case u.copied[i] >= 0:
			if u.copied[i] >= len(old) {
				return nil, internalError("a copy of the column at %v of a row of %v values", u.copied[i], len(old))
			}
			row[position], perRow = old[u.copied[i]], true
			if u.bugs.copySelf {
				// The planted bug.
				row[position] = old[position]
			}
		case u.computed[i] != nil:
			v, err := u.computed[i].Eval(old)
			if err != nil {
				return nil, fromExpr(err)
			}
			row[position], perRow = v.Proto(), true
		default:
			return nil, internalError("an assignment of no value")
		}
	}
	if !perRow {
		return row, nil
	}
	columns := u.table.GetColumns()
	for _, i := range u.order {
		position := u.positions[i]
		if row[position].HasNull() && (position == u.key || !columns[position].GetNullable()) {
			return nil, notNullFailed(u.table.GetTable(), columns[position].GetName())
		}
	}
	return row, nil
}

func (u *update) run(ctx context.Context) (int64, error) {
	name := u.table.GetTable()
	n, err := changeRows(ctx, u.w, name, u.key, u.src, u.changes, u.bugs, func(key []*s4qlpb.Value) error {
		old, err := u.w.get(ctx, name, key)
		if err != nil {
			return err
		}
		if old == nil {
			return internalError("the row of a key that an update found is gone")
		}
		row, err := u.newRow(old)
		if err != nil {
			if u.bugs.evalStops && errors.Is(err, expr.ErrEvaluation) {
				// The planted bug: the update stops here and succeeds.
				return errStopped
			}
			return err
		}
		same, err := compare(row[u.key], old[u.key])
		if err != nil {
			return err
		}
		given := row
		if u.bugs.oldValues {
			// The planted bug: the row as it was.
			given = old
		}
		if same == 0 {
			if err := u.w.upsert(ctx, name, row); err != nil {
				return err
			}
			u.ret.add(given)
			return nil
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
		u.ret.add(given)
		return nil
	})
	if errors.Is(err, errStopped) {
		return n, nil
	}
	return n, err
}

// errStopped ends an update with the planted bug evalStops, which then succeeds.
var errStopped = errors.New("stopped by the planted bug evalStops")

// remove deletes each row of its source, the rows that the statement finds, as for an [update], as
// [changeRows] says.
type remove struct {
	w     writer
	table *s4qlpb.CreateTable
	// key is the position of the column of the primary key of the table.
	key int
	src source
	// ret holds the rows of its RETURNING, or is nil for a delete without one.
	ret *returning
	// bugs planted in the delete, for the tests of this package.
	bugs bugs
}

func (r *remove) check(context.Context) error { return nil }

func (r *remove) run(ctx context.Context) (int64, error) {
	name := r.table.GetTable()
	return changeRows(ctx, r.w, name, r.key, r.src, changeOrder{}, r.bugs, func(key []*s4qlpb.Value) error {
		var old []*s4qlpb.Value
		if r.ret != nil {
			// The statement gives the values that the row had, so it reads them before it deletes it.
			var err error
			if old, err = r.w.get(ctx, name, key); err != nil {
				return err
			}
			if old == nil {
				return internalError("the row of a key that a delete found is gone")
			}
		}
		found, err := r.w.delete(ctx, name, key)
		if err != nil {
			return err
		}
		if !found {
			return internalError("the row of a key that a delete found is gone")
		}
		if !r.bugs.deleteNone {
			// Else the planted bug: the row is not given.
			r.ret.add(old)
		}
		return nil
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
// in the table with the name, whose key is the column at the position key, in the order that o
// says, and gives the number of rows. It adds the key of each row that src gives to a [keySet]
// first, and then calls change for each key of the set. So no cursor is open on the table while
// change changes it, which SQLite leaves undefined, and a row that change writes, also under a new
// key, is not in the set: each row that src gives is changed once, as SQLite changes each row that
// it finds once, also when change writes the columns that the conditions of src read. The set is in
// the storage, so a statement changes any number of rows without holding them in memory. The bugs
// b are those planted in the statement.
//
// Where o is in one pass, an error of a row of src, of its conditions, comes after the changes of
// the rows that src gave before it, as SQLite gives it when it changes each row as it finds it:
// changeRows changes those rows first, in the order of o, and gives the error of the first of them
// that fails, or else that of src. Where the tests force a path whose scan finds the rows in another
// order than o, the rows before that row are those of the path, as the rows whose conditions are
// evaluated at all are.
func changeRows(ctx context.Context, w writer, name string, key int, src source, o changeOrder, b bugs, change func(key []*s4qlpb.Value) error) (int64, error) {
	if b.changeNothing {
		// The planted bug: the statement finds no row.
		return 0, nil
	}
	if f, ok := src.(*filter); ok && f.again != nil {
		return changedOneByOne(ctx, key, f, change)
	}
	var n int64
	err := w.keys(ctx, name, o.by, func(k keySet) error {
		var found error
		for row, err := range src.rows(ctx) {
			if err != nil {
				if !o.onePass || !ofRow(err) {
					return err
				}
				found = err
				break
			}
			if err := k.add(ctx, o.keyOf(row, key)); err != nil {
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
		return found
	})
	return n, err
}

// changeOrder is the order in which an update changes the rows that it finds, which is SQLite's:
// SQLite changes each row as its access finds it, in one pass, unless the update sets a column of
// the primary key or of the index that the access reads by, its WHERE has a subquery, or it has
// RETURNING; and otherwise it collects the keys of the rows first and changes the rows in the order
// of the primary key. The access is the one that the rules choose, [chooseAccess], whatever path
// the tests force, so that the order does not depend on the path. The zero value is the order of
// the primary key, in which a delete deletes its rows, since no row of a delete fails on its own.
type changeOrder struct {
	// by is the key of the index whose order the rows change in, as [core.IndexKey] gives it, or nil
	// for the order of the primary key.
	by []core.KeyColumn
	// onePass is whether SQLite changes each row as it finds it, so that an error of the conditions
	// of a row comes after the changes of the rows before it, as [changeRows] says.
	onePass bool
}

// keyOf the row, whose primary key is the column at the position key, as the [keySet] of the order
// o takes it: the primary key, or the values of the columns of by.
func (o changeOrder) keyOf(row []*s4qlpb.Value, key int) []*s4qlpb.Value {
	if o.by == nil {
		return []*s4qlpb.Value{row[key]}
	}
	values := make([]*s4qlpb.Value, len(o.by))
	for i, c := range o.by {
		values[i] = row[c.Position]
	}
	return values
}

// ofRow reports whether err is an error of the conditions of one row: an evaluation error, or
// SQLite's `string or blob too big` of an expression that SQLite computes for each row.
func ofRow(err error) bool {
	var computed *computedTooBig
	return errors.Is(err, expr.ErrEvaluation) || errors.As(err, &computed)
}

// changeOrder of an update of the table t, whose scan s finds its rows for a filter of the
// conditions, and that sets the columns at the positions, with RETURNING if returning: SQLite's
// order, as [changeOrder] says, by the access of the rules, which is that of s where the tests force
// no path. Under a path that the tests force, it looks the indexes of the table up for the rules,
// unless no condition may serve an access, which reads every row by the primary key.
func (b build) changeOrder(ctx context.Context, t *s4qlpb.CreateTable, s *scan, conditions []condition, positions []int, returning bool) (changeOrder, error) {
	if returning || b.subs != nil || slices.ContainsFunc(positions, func(p int) bool { return slices.Contains(t.GetPrimaryKey(), uint32(p)) }) {
		return changeOrder{}, nil
	}
	a := s.access
	if b.how.path != (path{}) {
		a = access{}
		if serving := bounds(conditions); len(serving) > 0 {
			rules := b
			rules.how.path = path{}
			var err error
			if a, err = rules.access(ctx, t, serving); err != nil {
				return changeOrder{}, err
			}
		}
	}
	if a.none || a.point != nil || a.index == "" {
		return changeOrder{onePass: true}, nil
	}
	indexes, err := b.r.indexes(ctx, t.GetTable())
	if err != nil {
		return changeOrder{}, err
	}
	i := slices.IndexFunc(indexes, func(ix *s4qlpb.CreateIndex) bool { return ix.GetIndex() == a.index })
	if i < 0 {
		return changeOrder{}, internalError("the access of an update by the index %v, which its table does not have", a.index)
	}
	for _, c := range indexes[i].GetColumns() {
		if slices.Contains(positions, int(c.GetColumn())) {
			return changeOrder{}, nil
		}
	}
	return changeOrder{by: core.IndexKey(t, indexes[i]), onePass: true}, nil
}

// buildQuery: the source of the rows of the plan q, in the transaction of r, with args for the
// parameters of q, and with limit for the length of a text or a blob. It fails with SQLite's errors
// in SQLite's order: for a table that does not exist, for a column of the select list that the table
// does not have, for a column of a condition that it does not have, for a column of the ORDER BY
// that it does not have, and for a value of a condition, and then an argument of the LIMIT, that is
// longer than limit, as SQLite gives it when it binds an argument; and then with "not supported" for
// the first value of a condition, in the order of the text, that is of another type than its column,
// and then for an argument of the LIMIT that is not an integer from 0, as [limitCount] says. Its
// scan reads the rows as [build.access] says, by the reading how.
//
// A query with scalar subqueries looks the tables of its subqueries up after its own, in the order of
// the text, as SQLite finds every table of a select before any column, and the names of each
// subquery where the walk over its conditions reaches it, as [build.conditions] says. The lengths and
// the types of the values of a subquery are checked where its condition is in the text, and the
// value of each is computed after every check, before the access of the query is chosen; see
// [subqueries].
func buildQuery(ctx context.Context, r reader, q *Query, args []*s4qlpb.Value, limit int, how reading) (source, error) {
	b := build{r: r, args: args, limit: limit, how: how, subs: newSubqueries(q.Subqueries)}
	return b.node(ctx, q.Root)
}

// build of the operators of a query, in the transaction of r, as [buildQuery] says.
type build struct {
	r     reader
	args  []*s4qlpb.Value
	limit int
	how   reading
	// bugs planted in an update and a delete, for the tests of this package.
	bugs bugs
	// order is the ORDER BY and the LIMIT of the query whose rows are built, which its access is
	// chosen for.
	order orderBy
	// subs are the scalar subqueries of the statement, or nil for a statement without one.
	subs *subqueries
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
		// SQLite resolves the names of the select list before those of the WHERE, and those before the
		// names of the ORDER BY, so the nodes below the projection are built after it, in that order.
		inner := n.Source
		l, limited := inner.(*Limit)
		if limited {
			inner = l.Source
		}
		o, ordered := inner.(*Order)
		if ordered {
			inner = o.Source
		}
		f, filtered := inner.(*Filter)
		if filtered {
			inner = f.Source
		}
		if _, ok := inner.(*Scan); !ok {
			return nil, internalError("a projection of a node of type %T", inner)
		}
		src, err := b.node(ctx, inner)
		if err != nil {
			return nil, err
		}
		if err := b.subs.tables(ctx, b.r, src.(*scan).table); err != nil {
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
		if p.src, err = b.ordered(ctx, src.(*scan), f, o, l); err != nil {
			return nil, err
		}
		return p, nil
	}
	return nil, internalError("a query of a node of type %T", n)
}

// ordered rows of the scan s of a query, whose select list is resolved: the rows that pass the
// filter f, in the order of o, and the first of them that l says; each of the three is nil for a
// query without its clause. It makes the checks of [buildQuery] after those of the select list, in
// their order: the names of the conditions and then of the ORDER BY, the lengths of the values of
// the conditions and then of the argument of the LIMIT, and the types of the values of the
// conditions and then the argument of the LIMIT. Last it gives the scan its access, for the
// conditions, the ORDER BY, and the LIMIT.
func (b build) ordered(ctx context.Context, s *scan, f *Filter, o *Order, l *Limit) (source, error) {
	var conditions []condition
	if f != nil {
		var err error
		if _, conditions, err = b.conditions(ctx, f, s); err != nil {
			return nil, err
		}
	}
	if o != nil {
		// SQLite looks the columns of the ORDER BY up in the order of the text.
		columns, err := orderColumns(s.table, nil, o, nil)
		if err != nil {
			return nil, err
		}
		b.order.columns = columns
	}
	b.order.limited = l != nil
	b.order.turnMixed, b.order.ascendLast, b.order.skipGap = b.how.bugs.turnMixed, b.how.bugs.ascendLast, b.how.bugs.skipGap
	if err := b.subs.uncorrelated(); err != nil {
		return nil, err
	}

	if f != nil {
		if err := b.values(f, conditions); err != nil {
			return nil, err
		}
	}
	var count *s4qlpb.Value
	if l != nil {
		var err error
		if count, err = eval(l.Count, b.args); err != nil {
			return nil, err
		}
		if err := checkLength(count, b.limit); err != nil {
			return nil, err
		}
	}
	// Without a filter, there are no conditions, and types checks none.
	if err := b.types(f, s, conditions); err != nil {
		return nil, err
	}
	var n int64
	if l != nil {
		var err error
		if n, err = limitCount(l.Count, count); err != nil {
			return nil, err
		}
	}
	if f != nil {
		if err := b.compute(ctx, f, s, conditions, l != nil && n == 0); err != nil {
			return nil, err
		}
	}

	var src source = s
	if f != nil || o != nil {
		if err := b.reach(ctx, s, conditions); err != nil {
			return nil, err
		}
	}
	if f != nil {
		src = &filter{src: s, conditions: conditions}
	}
	switch {
	case l != nil && b.how.bugs.limitFirst && s.access.order == sorted && len(s.access.by) > 0:
		// The planted bug limitFirst: the first rows of the access, unsorted, sorted after the LIMIT.
		by := s.access.by
		s.access.by = nil
		src = &sortedAfter{src: &limit{src: src, n: n}, by: by}
	case l != nil:
		src = &limit{src: src, n: n}
	}
	return src, nil
}

// filter of the rows of src, a scan, by the conditions of f: the names of [build.conditions], and
// then the values of [build.bind].
func (b build) filter(ctx context.Context, f *Filter, src source) (source, error) {
	s, conditions, err := b.conditions(ctx, f, src)
	if err != nil {
		return nil, err
	}
	return b.bind(ctx, f, s, conditions)
}

// conditions of the filter f of the rows of src, a scan, with the position of the column of each, and
// no value yet. It looks the columns of the conditions up as [build.walk] says.
func (b build) conditions(ctx context.Context, f *Filter, src source) (*scan, []condition, error) {
	s, ok := src.(*scan)
	if !ok {
		return nil, nil, internalError("a filter of a node of type %T", src)
	}
	conditions, err := b.walk(ctx, f, s.table, nil)
	if err != nil {
		return nil, nil, err
	}
	return s, conditions, nil
}

// walk over the conditions of the filter f of the rows of the table t: the conditions, with the
// position of the column of each, and no value yet. It looks the columns of the conditions up in the
// order of the text, as SQLite does: the first comparison of a column that the table does not have
// fails with `no such column`. A test for NULL of such a column does not stop SQLite, which goes on
// to the next condition with the error of that column: a later test for NULL of a column that the
// table does not have replaces the error, and the next comparison ends the walk with it, before
// SQLite looks its own column up.
//
// A comparison with a scalar subquery first fails, in a select, with SQLite's `row value misused` for
// a subquery of `*` of more than one column, as SQLite checks the widths of a comparison before its
// operands, and then looks the names of the subquery up where the text has it: before the column for
// a subquery on the left, and after it otherwise, as [subqueries.names] says.
//
// For the conditions of a subquery, outer is the table of the statement, and a column that t does not
// have and outer has is correlated: SQLite takes it for the column of outer, so the walk goes on, and
// the subqueries refuse the statement once SQLite would have given every error of its names, as
// [subqueries.uncorrelated] says. Its position is -1. For the conditions of the statement outer is
// nil.
func (b build) walk(ctx context.Context, f *Filter, t, outer *s4qlpb.CreateTable) ([]condition, error) {
	of := columnPositions(t)
	conditions := make([]condition, len(f.Conditions))
	// n.pending is the column that the walk goes on with, of a test for NULL or of the argument of a
	// call, as [resolver] says, or nil.
	n := &resolver{found: func(name stmt.Name) bool {
		_, ok := b.subs.lookup(of, outer, name)
		return ok
	}}
	for i, c := range f.Conditions {
		// A condition that tests an expression has no column of its own: its names are those of the
		// expression, which come where the text has them.
		position, ok := -1, true
		if !c.Expression.Present() {
			position, ok = b.subs.lookup(of, outer, c.Column)
		}
		switch c.Operator {
		case stmt.OpIsNull, stmt.OpIsNotNull:
			if c.Value != nil {
				return nil, internalError("a test for NULL with a value")
			}
			if !ok {
				n.pending = &f.Conditions[i].Column
			}
		case stmt.OpEqual, stmt.OpNotEqual, stmt.OpLess, stmt.OpLessOrEqual, stmt.OpGreater, stmt.OpGreaterOrEqual:
			if c.Value == nil {
				return nil, internalError("a comparison without a value")
			}
			sub, subquery := c.Value.(*Subquery)
			var in *inner
			if subquery {
				var err error
				if in, err = b.subs.at(sub, outer); err != nil {
					return nil, err
				}
				if in.width > 1 {
					// Only a select has found the table of a subquery, and so its width, before the walk
					// reaches it: an update and a delete find it after, as subqueries.names does, and
					// fail with subqueries.widths. SQLite gives the offset of the column on the left of
					// the comparison, and none (-1) for the subquery on the left.
					offset := c.Column.Offset
					switch {
					case c.ValueFirst:
						offset = -1
					case c.Expression.Present():
						offset = leftmostOffset(c.Expression)
					}
					return nil, rowValueMisused(offset)
				}
			}
			if n.pending != nil {
				return nil, noSuchResultColumn(*n.pending)
			}
			// SQLite resolves the names of the two sides of a comparison in the order of the text. A
			// subquery after a pending column looks its table and its column up, and then ends the walk
			// with the pending column, as [subqueries.pendingNames] says.
			subNames := func() error {
				if n.pending != nil {
					return b.subs.pendingNames(ctx, b, in, *n.pending)
				}
				return b.subs.names(ctx, b, in)
			}
			if subquery && c.ValueFirst {
				if err := subNames(); err != nil {
					return nil, err
				}
			}
			arithmetic, _ := c.Value.(*Arithmetic)
			if arithmetic != nil && c.ValueFirst {
				if name, stop := n.resolve(arithmetic.Expression); stop {
					return nil, noSuchResultColumn(name)
				}
			}
			if c.Expression.Present() {
				if name, stop := n.resolve(c.Expression); stop {
					return nil, noSuchResultColumn(name)
				}
			}
			if !ok {
				return nil, noSuchResultColumn(c.Column)
			}
			if arithmetic != nil && !c.ValueFirst {
				if name, stop := n.resolve(arithmetic.Expression); stop {
					return nil, noSuchResultColumn(name)
				}
			}
			if subquery && !c.ValueFirst {
				if err := subNames(); err != nil {
					return nil, err
				}
			}
			if plain := !subquery && arithmetic == nil; plain && !c.ValueFirst && n.pending != nil {
				// A literal or a parameter after a pending column ends the walk.
				return nil, noSuchResultColumn(*n.pending)
			}
		default:
			return nil, internalError("a condition with the operator %v", int(c.Operator))
		}
		conditions[i] = condition{position: position, op: c.Operator, valueFirst: c.ValueFirst}
	}
	if n.pending != nil {
		return nil, noSuchResultColumn(*n.pending)
	}
	return conditions, nil
}

// columnPositions of the table t, by the fold of the name of each column, as [positionsOf] gives
// them.
func columnPositions(t *s4qlpb.CreateTable) map[string]int {
	var names []string
	for _, c := range t.GetColumns() {
		names = append(names, c.GetName())
	}
	return positionsOf(names)
}

// orderColumns of the ORDER BY o of a query of the table t: the position of each column and its
// direction, in the order of the text, or SQLite's `no such column` for the first that t does not
// have. For a subquery, outer is the table of the statement, and a correlated column is taken as
// [build.walk] says, with the subqueries subs; for a statement outer and subs are nil.
func orderColumns(t, outer *s4qlpb.CreateTable, o *Order, subs *subqueries) ([]core.KeyColumn, error) {
	of := columnPositions(t)
	var columns []core.KeyColumn
	for _, c := range o.Columns {
		position, ok := subs.lookup(of, outer, c.Column)
		if !ok {
			return nil, noSuchResultColumn(c.Column)
		}
		columns = append(columns, core.KeyColumn{Position: position, Descending: c.Descending})
	}
	return columns, nil
}

// bind the values of the conditions of the filter f to conditions, which [build.conditions] gave for
// the scan s, and give the filter of the rows of s. It evaluates each value, and gives SQLite's error
// for one that is longer than the limit, as [build.values] says, and then "not supported" for the
// first value, in the order of the text, that is of another type than its column, as [build.types]
// says. Then it computes the value of each subquery, as [build.compute] says. Last it gives the scan
// its access, as [build.reach] says, so the errors do not depend on the access.
func (b build) bind(ctx context.Context, f *Filter, s *scan, conditions []condition) (source, error) {
	if err := b.values(f, conditions); err != nil {
		return nil, err
	}
	if err := b.types(f, s, conditions); err != nil {
		return nil, err
	}
	return b.computeAndReach(ctx, f, s, conditions)
}

// computeAndReach is the end of [build.bind], once the values and their types are checked: it computes
// the constants and the subqueries, gives the scan its access, and gives the filter.
func (b build) computeAndReach(ctx context.Context, f *Filter, s *scan, conditions []condition) (source, error) {
	if err := b.compute(ctx, f, s, conditions, false); err != nil {
		return nil, err
	}
	if err := b.reach(ctx, s, conditions); err != nil {
		return nil, err
	}
	filtered := &filter{src: s, conditions: conditions}
	if b.how.bugs.ownChanges && b.subs != nil {
		// The planted bug: the subqueries are computed again, on the rows as the statement left them.
		filtered.again = func(ctx context.Context) error {
			if err := b.compute(ctx, f, s, conditions, false); err != nil {
				return err
			}
			return b.reach(ctx, s, conditions)
		}
	}
	return filtered, nil
}

// constants computes the value of each condition of the table t whose value is an expression without
// a column, in the order of the text: once for each run of the statement, after its checks, so that
// its value serves the access as an argument does, and its error comes also where no row is read. The
// sink of a write that is built only to look its names up, in a [readOnly] transaction, computes
// none, and each such value is NULL.
//
// Text with a call that is too long is the one exception, since SQLite computes such an expression
// when the first row reaches it, unless its access needs it before. Where the condition may serve the
// access, as [mayServe] says, SQLite's `string or blob too big` comes here, as for every other
// constant; otherwise the condition keeps the error, serves no access, and gives the error on the
// first row that it is evaluated on. A nil table t is that of conditions that serve no access.
func (b build) constants(ctx context.Context, t *s4qlpb.CreateTable, conditions []condition) error {
	_, lookups := b.r.(readOnly)
	for i := range conditions {
		c := &conditions[i]
		if c.constant == nil {
			continue
		}
		if lookups {
			c.value = null()
			continue
		}
		v, err := c.constant.Eval(nil)
		if err == nil {
			c.value = v.Proto()
			continue
		}
		if c.lazy && errors.Is(err, expr.ErrTooLong) {
			serves, indexErr := mayServe(ctx, b.r, t, *c)
			if indexErr != nil {
				return indexErr
			}
			if !serves {
				c.failed = fromExpr(err)
				continue
			}
		}
		return fromExpr(err)
	}
	return nil
}

// mayServe reports whether SQLite may compute the value of the condition c of the table t, which
// compares a column alone with a constant, before it reads a row, to find the rows by the key or by an
// index: where the operator is not != and the column is the key or a column of an index of t. No
// condition of a nil table serves.
func mayServe(ctx context.Context, r reader, t *s4qlpb.CreateTable, c condition) (bool, error) {
	if t == nil || c.test != nil || c.per != nil || c.paired || c.position < 0 || c.op == stmt.OpNotEqual {
		return false, nil
	}
	for _, k := range t.GetPrimaryKey() {
		if int(k) == c.position {
			return true, nil
		}
	}
	indexes, err := r.indexes(ctx, t.GetTable())
	if err != nil {
		return false, err
	}
	for _, ix := range indexes {
		for _, column := range ix.GetColumns() {
			if int(column.GetColumn()) == c.position {
				return true, nil
			}
		}
	}
	return false, nil
}

// hoist computes the subtrees of the expressions of the conditions of the table t that SQLite
// computes before any row, as [hoisted] says, in the order of the text, but in a [readOnly]
// transaction, where SQLite refuses a write first. For the conditions of a subquery, which the
// statement may not compute, it computes those subtrees of the constants of text too: of a constant
// that may serve the access of the subquery, as [mayServe] says, which SQLite codes in place where
// the subquery seeks, its operands, and of any other constant the constant itself where it has no
// call, as for a side of a comparison. Each gives SQLite's `string or blob too big` where it is too
// long; the value of a constant is computed again with the subquery.
func (b build) hoist(ctx context.Context, t *s4qlpb.CreateTable, conditions []condition, subquery bool) error {
	if _, lookups := b.r.(readOnly); lookups {
		return nil
	}
	for _, c := range conditions {
		if err := computeHoisted(c.hoisted); err != nil {
			return err
		}
		if !subquery || !c.text.Present() {
			continue
		}
		serves, err := mayServe(ctx, b.r, t, c)
		if err != nil {
			return err
		}
		hoisted, err := bindHoisted(t, c.text, serves, b.args, b.limit)
		if err != nil {
			return err
		}
		if err := computeHoisted(hoisted); err != nil {
			return err
		}
	}
	return nil
}

// values of the conditions of the filter f, which it evaluates and puts in conditions: SQLite's error
// for the first, in the order of the text, that is longer than the limit. The values of a subquery
// are checked where it is, as [subqueries.lengths] says, and its own value is computed later. An
// expression is computed later, where a text that it makes may be too long, as [hoisted] says, but
// the arguments of its parameters are checked here, on either side of the condition, as
// [argumentLengths] says.
func (b build) values(f *Filter, conditions []condition) error {
	for i, c := range f.Conditions {
		if !c.ValueFirst {
			if err := argumentLengths(c.Expression, b.args, b.limit); err != nil {
				return err
			}
		}
		switch value := c.Value.(type) {
		case nil:
		case *Arithmetic:
			if err := argumentLengths(value.Expression, b.args, b.limit); err != nil {
				return err
			}
		case *Subquery:
			if err := b.subs.lengths(b, value); err != nil {
				return err
			}
		default:
			v, err := eval(c.Value, b.args)
			if err != nil {
				return err
			}
			if err := checkLength(v, b.limit); err != nil {
				return err
			}
			conditions[i].value = v
		}
		if c.ValueFirst {
			if err := argumentLengths(c.Expression, b.args, b.limit); err != nil {
				return err
			}
		}
	}
	return nil
}

// types of the values of the conditions of the filter f of the scan s: "not supported" for the first
// value, in the order of the text, that is of another type than its column. A subquery is checked
// where it is, as [subqueries.types] says.
//
// An expression is checked and bound first, as [bindExpression] says, the side that the condition
// tests and its value in the order of the text, and then the class of its values must be that of the
// other side, as [comparable] says: so a column of type TEXT or BLOB compared with arithmetic is
// refused, whatever the arguments, where SQLite converts the number to text or a blob by the column's
// affinity, and a number compared with text, where SQLite orders a number before text. The
// expressions are put in conditions, to be computed later.
func (b build) types(f *Filter, s *scan, conditions []condition) error {
	for i := range conditions {
		c, fc := &conditions[i], f.Conditions[i]
		arithmetic, _ := fc.Value.(*Arithmetic)
		if arithmetic != nil && arithmetic.Expression.Kind == stmt.ExprColumn && !fc.Expression.Present() {
			// A column compared with a column alone, of the same class, compares them as SQLite does,
			// with no conversion and no evaluator.
			pair, ok := columnPositions(s.table)[arithmetic.Expression.Column.Fold()]
			if !ok {
				return internalError("a condition of the column %v, which the table %v does not have", arithmetic.Expression.Column.Value, s.table.GetTable())
			}
			if c.position < 0 {
				return internalError("a comparison with the column %v of a condition of no column", arithmetic.Expression.Column.Value)
			}
			columns := s.table.GetColumns()
			if columnClass(columns[pair].GetType()) != columnClass(columns[c.position].GetType()) {
				return fmt.Errorf("%w: %v compared with %v", stmt.ErrNotSupported, columnName(s.table, pair), columnName(s.table, c.position))
			}
			c.paired, c.pair, c.nullCompares = true, pair, b.how.bugs.nullCompares
			continue
		}
		if fc.Expression.Present() || arithmetic != nil {
			if err := b.arithmetic(fc, s.table, c, arithmetic); err != nil {
				return err
			}
		}
		if sub, ok := fc.Value.(*Subquery); ok {
			tested := classAny
			if c.test != nil {
				tested = expressionClass(fc.Expression, c.test)
			}
			if err := b.subs.types(b, sub, s.table, c.position, c.test != nil, tested); err != nil {
				return err
			}
			continue
		}
		if arithmetic != nil || c.value == nil {
			continue
		}
		if c.test != nil {
			if tested := expressionClass(fc.Expression, c.test); !comparable(valueClass(c.value), tested) {
				what, err := valueName(fc.Value, c.value)
				if err != nil {
					return err
				}
				return fmt.Errorf("%w: %v compared with %v", stmt.ErrNotSupported, what, expressionName(tested))
			}
			continue
		}
		if err := checkType(s.table, c.position, c.value, fc.Value, "compared with"); err != nil {
			return err
		}
	}
	return nil
}

// arithmetic of the condition fc of the table t, whose value is the expression value or not an
// expression: it binds the side that fc tests and its value, in the order of the text, into the
// condition c, and refuses a value whose class is not that of the side that it is compared with.
func (b build) arithmetic(fc Condition, t *s4qlpb.CreateTable, c *condition, value *Arithmetic) error {
	var err error
	c.nullCompares, c.errorSkipsRow = b.how.bugs.nullCompares, b.how.bugs.errorSkipsRow
	defer func() { c.unserved = b.how.bugs.constantsServeNothing && c.constant != nil }()
	bindValue := func() error {
		if value == nil {
			return nil
		}
		bound, err := bindExpression(t, value.Expression, b.args, b.limit)
		if err != nil {
			return err
		}
		if value.Expression.HasColumn() {
			c.per = bound
		} else {
			text, call := isText(value.Expression), hasCall(value.Expression)
			c.constant, c.lazy = bound, text && call
			if text {
				c.text = value.Expression
			}
		}
		return nil
	}
	if fc.ValueFirst {
		if err := bindValue(); err != nil {
			return err
		}
	}
	if fc.Expression.Present() {
		if c.test, err = bindExpression(t, fc.Expression, b.args, b.limit); err != nil {
			return err
		}
	}
	if !fc.ValueFirst {
		if err := bindValue(); err != nil {
			return err
		}
	}
	// The subtrees of the sides with a column; those of a constant are bound where it is computed, as
	// [build.hoist] says.
	sides := []stmt.Expression{fc.Expression}
	if value != nil && value.Expression.HasColumn() {
		sides = append(sides, value.Expression)
	}
	for _, e := range sides {
		hoisted, err := bindHoisted(t, e, false, b.args, b.limit)
		if err != nil {
			return err
		}
		c.hoisted = append(c.hoisted, hoisted...)
	}
	if value == nil {
		return nil
	}
	bound := c.per
	if bound == nil {
		bound = c.constant
	}
	class := expressionClass(value.Expression, bound)
	var meets bool
	var name string
	if c.test != nil {
		tested := expressionClass(fc.Expression, c.test)
		meets, name = comparable(tested, class), expressionName(tested)
	} else {
		meets, name = comparable(columnClass(t.GetColumns()[c.position].GetType()), class), columnName(t, c.position)
	}
	if meets {
		return nil
	}
	return fmt.Errorf("%w: %v compared with %v", stmt.ErrNotSupported, expressionName(class), name)
}

// reach the rows of the scan s for a filter of the conditions and the ORDER BY of the build: give the
// scan its access, as [build.access] says. An access that delivers the order of the ORDER BY reads in
// the direction that delivers it, and every other in the direction of the reading: the scan of a
// statement without ORDER BY backward or forward, and the rows that tie in a sort in the reverse of
// the order of the primary key or in that order.
func (b build) reach(ctx context.Context, s *scan, conditions []condition) error {
	a, err := b.access(ctx, s.table, bounds(conditions))
	if err != nil {
		return err
	}
	if a.order != delivered {
		a.backward = b.how.backward
	}
	s.access = a
	return nil
}

// access to the rows of the table t for a filter of the conditions and the ORDER BY and the LIMIT of
// the build: the one that [chooseAccess] gives, or the one of the path that the tests force, as
// [forcedAccess] gives it. It reads the indexes of the table only if the access depends on them, so
// a statement that the rules give no row, or the row of a primary key, reads none.
func (b build) access(ctx context.Context, t *s4qlpb.CreateTable, conditions []condition) (access, error) {
	p, o := b.how.path, b.order
	// rules is what the rules choose for: the statement without its ORDER BY, for a path that sorts.
	rules := o
	if p.sort {
		rules = orderBy{}
	}
	switch p.kind {
	case pathFull:
		return p.place(o, t, core.IndexKey(t, nil), conditions, access{}), nil
	case pathPrimary:
		a, _, err := forcedAccess(t, nil, conditions, o, p, b.how.bugs)
		return a, err
	case pathRules:
		if a, err := chooseAccess(t, nil, conditions, rules, b.how.bugs); err != nil || a.none || a.point != nil {
			return o.sort(t, a), err
		}
	}
	indexes, err := b.r.indexes(ctx, t.GetTable())
	if err != nil {
		return access{}, err
	}
	if b.how.bugs.lastIndexWins {
		// The planted bug lastIndexWins.
		indexes = slices.Clone(indexes)
		slices.Reverse(indexes)
	}
	if p.kind == pathIndex {
		if a, ok, err := forcedAccess(t, indexes, conditions, o, p, b.how.bugs); err != nil || ok {
			return a, err
		}
	}
	a, err := chooseAccess(t, indexes, conditions, rules, b.how.bugs)
	if err != nil || !p.sort {
		return a, err
	}
	return o.sort(t, a), nil
}

// buildValues: the source of the rows of v, with args for their parameters, and the expressions of
// each row. The value at the index i of a row of v is for the column at positions[i] of a table of
// the columns, and each row that it gives has a value for each column of the table, in the order of
// the table, with the default of a column that positions do not name, as SQLite gives a column that
// an insert leaves out, or NULL if the column has none. Its expression is nil. A NULL that the
// statement gives is NULL, whatever the default of its column.
func buildValues(v *Values, args []*s4qlpb.Value, positions []int, columns []*s4qlpb.Column) (*values, [][]Expr, error) {
	src := &values{}
	var exprs [][]Expr
	width := len(columns)
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
			switch c := columns[position]; {
			case value != nil:
			case c.HasDefault():
				data[position] = c.GetDefault()
			default:
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
// list, then another number of values than the columns of its list, or than the columns of the
// table if it has no list, and then a column of its RETURNING that the table does not have, as
// [buildReturning] says; an upsert has the errors of the names of its clause before those of RETURNING,
// as [conflictNames] says, and those of the lengths of DO UPDATE after those of its row, as
// [build.doUpdate] says. For INSERT it then
// makes the check that SQLite makes when it binds the arguments: a text or a blob that is longer
// than limit bytes gets SQLite's `string or blob too big`. It checks a literal so too, which SQLite
// meets in its text only with a limit below the length of the text: see the README. For UPDATE and
// DELETE, see [build.change]; their scan reads the rows as [build.access] says, by the reading how.
// For CREATE INDEX and DROP INDEX, see [buildCreateIndex] and [buildDropIndex].
func buildSink(ctx context.Context, w writer, p Plan, args []*s4qlpb.Value, b bugs, limit int, how reading) (sink, error) {
	switch p := p.(type) {
	case *Update:
		return build{r: w, args: args, limit: limit, how: how, bugs: b, subs: newSubqueries(p.Subqueries)}.change(ctx, w, p.Source, p.Set, p.Returning, true)
	case *Delete:
		return build{r: w, args: args, limit: limit, how: how, bugs: b, subs: newSubqueries(p.Subqueries)}.change(ctx, w, p.Source, nil, p.Returning, false)
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
			if c.Default != nil {
				// The literal of the DEFAULT, which the pass gives only of the type of its column, or
				// NULL. The storage keeps a -0.0 as 0.0, which is what SQLite stores for it.
				v, err := eval(c.Default, nil)
				if err != nil {
					return nil, err
				}
				column.Default = v
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
		key := int(t.GetPrimaryKey()[0])
		if p.Conflict != nil {
			if err := conflictNames(t, key, p.Conflict); err != nil {
				return nil, err
			}
		}
		ret, err := buildReturning(t, p.Returning, b)
		if err != nil {
			return nil, err
		}
		src, exprs, err := buildValues(v, args, positions, t.GetColumns())
		if err != nil {
			return nil, err
		}
		if p.Columns != nil {
			for position, c := range t.GetColumns() {
				switch {
				case named[position]:
				case b.zeroLeftOut && c.GetNullable() && !c.HasDefault():
					// The planted bug: a column that takes NULL, has no default, and that the list leaves
					// out gets the zero value of its type.
					src.data[0][position] = zeroOf(c.GetType())
				case b.nullDefault && c.HasDefault():
					// The planted bug: a column with a default that the list leaves out gets NULL.
					src.data[0][position] = null()
				}
			}
		}
		// SQLite checks the length of a value that the statement gives, a literal or an argument, when
		// it binds it. A default is only in the record of the row, which the storage measures.
		for n, row := range src.data {
			for position, v := range row {
				if exprs[n][position] == nil {
					continue
				}
				if err := checkLength(v, limit); err != nil {
					return nil, err
				}
			}
		}
		i := insert{w: w, table: t, key: key, src: src, exprs: exprs, ret: ret, skipUnique: b.skipUnique, convertReal: b.convertReal}
		if p.Conflict == nil {
			return &i, nil
		}
		u := &upsert{insert: i, nothing: p.Conflict.Action == stmt.DoNothing}
		if !u.nothing {
			if u.set, u.where, err = (build{r: w, args: args, limit: limit, how: how}).doUpdate(ctx, w, t, p.Conflict); err != nil {
				return nil, err
			}
		}
		return u, nil
	}
	return nil, internalError("a plan of type %T", p)
}

// change is the sink of an UPDATE, if isUpdate, with the assignments set, or of a DELETE, that
// changes the rows of source, a [*Scan], or a [*Filter] of one, in the transaction of w. It fails with
// SQLite's errors in SQLite's order: for a table that does not exist; for a column of the SET that
// the table does not have, the first in the order of the text, with the value of its name and
// nothing about quotes, as SQLite's update itself gives it; for a column of the WHERE, as
// [build.conditions] says; for a column of the RETURNING returning, as [buildReturning] says; and for
// a value of the SET and then of the WHERE that is longer than
// the limit, as SQLite gives it when it binds an argument, the first in the order of the text, which
// is the order of the parameters too. Then it fails with "not supported" for a value of the WHERE of
// another type than its column, as a select does, and gives the scan its access, as
// [build.bind] says. A table without a WHERE is read as the reading says too.
//
// A scalar subquery of the WHERE looks its table and its names up where the walk over the WHERE
// reaches it, as [build.walk] says, and fails a subquery of `*` of more than one column after the
// names of the WHERE, before those of RETURNING, as [subqueries.widths] says; a correlated subquery
// is refused after RETURNING. Its value is computed before the change reads or changes a row, as
// [build.compute] says, so it is computed from the rows as they are before the statement.
func (b build) change(ctx context.Context, w writer, source Node, set []Assignment, returning *Returning, isUpdate bool) (sink, error) {
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
		// SQLite resolves the names of the value of an assignment before its column, as a select
		// resolves them, with the question about quotes.
		if arithmetic, ok := a.Value.(*Arithmetic); ok {
			n := &resolver{found: func(name stmt.Name) bool { _, ok := of[name.Fold()]; return ok }}
			if name, missing := n.all(arithmetic.Expression); missing {
				return nil, noSuchResultColumn(name)
			}
		}
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
		b.subs.from(t)
		if _, conditions, err = b.conditions(ctx, f, s); err != nil {
			return nil, err
		}
		if err := b.subs.widths(f); err != nil {
			return nil, err
		}
	}
	ret, err := buildReturning(t, returning, b.bugs)
	if err != nil {
		return nil, err
	}
	if err := b.subs.uncorrelated(); err != nil {
		return nil, err
	}
	u := &update{w: w, table: t, key: int(key[0]), positions: positions, values: make([]*s4qlpb.Value, len(set)), exprs: make([]Expr, len(set)),
		computed: make([]*expr.Bound, len(set)), copied: make([]int, len(set)), ret: ret, bugs: b.bugs}
	for i, a := range set {
		u.exprs[i], u.copied[i] = a.Value, -1
		if arithmetic, ok := a.Value.(*Arithmetic); ok {
			// An expression is computed later, but the arguments of its parameters are checked here.
			if err := argumentLengths(arithmetic.Expression, b.args, b.limit); err != nil {
				return nil, err
			}
			continue
		}
		v, err := eval(a.Value, b.args)
		if err != nil {
			return nil, err
		}
		if err := checkLength(v, b.limit); err != nil {
			return nil, err
		}
		u.values[i] = v
	}
	if filtered {
		if err := b.values(f, conditions); err != nil {
			return nil, err
		}
	}
	// The expressions of SET, in the order of the text, after the lengths of SET and of WHERE, as
	// [build.types] checks those of WHERE, and then those. In a read-only transaction, SQLite refuses
	// the write before it looks at a value of SET, as it does for a plain value, which [update.check]
	// checks, so the expressions of SET are neither checked nor computed there.
	_, lookups := w.(readOnly)
	if !lookups {
		if err := u.bind(of, set, b.args, b.limit); err != nil {
			return nil, err
		}
	}
	if filtered {
		if err := b.types(f, s, conditions); err != nil {
			return nil, err
		}
	}
	// The expressions without a column of SET are computed once, after the checks, as those of WHERE
	// are, before them, and before them the subtrees that SQLite computes before any row. In a
	// read-only transaction none is bound, and none is computed. Text without a column that is too
	// long is computed for each row, as SQLite computes it, and fails on the first.
	if err := computeHoisted(u.hoisted); err != nil {
		return nil, err
	}
	for i, bound := range u.computed {
		if bound == nil {
			continue
		}
		e := set[i].Value.(*Arithmetic).Expression
		if e.HasColumn() {
			continue
		}
		v, err := bound.Eval(nil)
		if err != nil {
			if isText(e) && errors.Is(err, expr.ErrTooLong) {
				continue
			}
			return nil, fromExpr(err)
		}
		u.values[i] = v.Proto()
	}
	if filtered {
		if src, err = b.computeAndReach(ctx, f, s, conditions); err != nil {
			return nil, err
		}
	}
	if !isUpdate {
		return &remove{w: w, table: t, key: int(key[0]), src: src, ret: ret, bugs: b.bugs}, nil
	}
	if len(set) == 0 {
		return nil, internalError("an update that sets no column")
	}
	u.src = src
	if u.changes, err = b.changeOrder(ctx, t, s, conditions, positions, returning != nil); err != nil {
		return nil, err
	}
	u.order = make([]int, len(u.positions))
	for i := range u.order {
		u.order[i] = i
	}
	slices.SortFunc(u.order, func(a, b int) int { return cmp.Compare(u.positions[a], u.positions[b]) })
	return u, nil
}

// bind the values of the assignments set, whose columns are at the positions of the update in a table
// whose columns are of, that are expressions, in the order of the text: a column alone, as in `set a
// = b`, is copied from the row, and every other expression is checked and bound to the arguments args
// under the length limit, as [bindExpression] says, with its subtrees that [hoisted] gives.
func (u *update) bind(of map[string]int, set []Assignment, args []*s4qlpb.Value, limit int) error {
	for i, a := range set {
		arithmetic, ok := a.Value.(*Arithmetic)
		if !ok {
			continue
		}
		if e := arithmetic.Expression; e.Kind == stmt.ExprColumn {
			position, ok := of[e.Column.Fold()]
			if !ok {
				return internalError("an assignment of the column %v, which the table does not have", e.Column.Value)
			}
			u.copied[i] = position
			continue
		}
		bound, err := bindExpression(u.table, arithmetic.Expression, args, limit)
		if err != nil {
			return err
		}
		u.computed[i] = bound
		hoisted, err := bindHoisted(u.table, arithmetic.Expression, true, args, limit)
		if err != nil {
			return err
		}
		u.hoisted = append(u.hoisted, hoisted...)
	}
	return nil
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
