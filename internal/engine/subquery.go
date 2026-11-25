package engine

import (
	"context"
	"fmt"

	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// The scalar subqueries of slice 15. A subquery stands as the value of a comparison of the WHERE of
// a select, an update, or a delete. Its value is the value of its one column in the first row that it
// gives, or NULL if it gives none, and the engine computes it once for each run of the statement, in
// the transaction of the statement, before the statement reads or changes a row of its own table: so
// an update or a delete on the table that the subquery reads sees the rows as they were before it.
//
// Its operators are built as a query's are, by the rules of [chooseAccess] for its conditions and its
// ORDER BY, as a query with a LIMIT, since only its first row is read: it reads and holds no more than
// a select of its text with `limit 1`. Where its ORDER BY does not decide which row is first, the
// first row is the first of its access, which reads forward or backward as the engine draws it for
// a query, one draw for each subquery, so that a caller that relies on SQLite's first row is seen:
// see [build.compute].
//
// The build checks a subquery in SQLite's order of errors, which differs between a select and an
// update or a delete:
//
//   - In a select, SQLite finds every table of the statement before any column: its own, and then
//     those of its subqueries, in the order of the text, as [subqueries.tables] does. It expands `*`
//     of each subquery then, so a subquery of `*` of a table of more than one column fails with `row
//     value misused` at its comparison, as [build.walk] says.
//   - In an update and a delete, SQLite finds the table of a subquery when its walk over the WHERE
//     reaches it, and fails a subquery of `*` of more than one column after the names of the WHERE,
//     as [subqueries.widths] says.
//
// In both, the names of a subquery come where the text has it: the name of its column, then those of
// its conditions, and then those of its ORDER BY, as [subqueries.names] looks them up. A name of a
// subquery that its table does not have and the table of the statement has is correlated, which
// SQLite takes, and the slice does not: the statement is refused once SQLite would have given every
// error of its names, as [subqueries.uncorrelated] says. The lengths of the values of a subquery, its
// argument of LIMIT among them, are checked where it is in the text, as [subqueries.lengths] says,
// and so are their types, as [subqueries.types] says.

// subqueries of a statement, as the build of its operators finds them, one [inner] for each, in the
// order of the text. A nil *subqueries is the subqueries of a statement without one.
type subqueries struct {
	list []*inner
	// outer is the table of the statement, once it is found.
	outer *s4qlpb.CreateTable
	// correlated is the first name of a subquery that is correlated, or nil.
	correlated *stmt.Name
}

// inner is a subquery that is being built: its plan, its parts, and what each step of the build has
// found.
type inner struct {
	number int
	// query is the plan of the subquery.
	query   *Query
	project *Project
	filter  *Filter
	order   *Order
	limit   *Limit
	table   *Scan
	// s is the scan of its table, once the table is found.
	s *scan
	// position of its column in its table, and width, the number of columns that it gives: 1, or, for
	// `*`, the number of columns of its table.
	position, width int
	// conditions of its filter, its ORDER BY and LIMIT for the rules of its access, and the argument of
	// its LIMIT and its count.
	conditions []condition
	by         orderBy
	count      *s4qlpb.Value
	n          int64
	// named is set once its names are looked up.
	named bool
}

// newSubqueries of a statement whose subqueries are qs, or nil for a statement without one. It panics
// for a subquery of another shape than [Query] says.
func newSubqueries(qs []*Query) *subqueries {
	if qs == nil {
		return nil
	}
	s := &subqueries{}
	for i, q := range qs {
		in := &inner{number: i + 1, query: q}
		p, ok := q.Root.(*Project)
		if !ok || q.Subqueries != nil {
			panic(fmt.Sprintf("engine: a subquery of a root of type %T", q.Root))
		}
		in.project = p
		n := p.Source
		if l, ok := n.(*Limit); ok {
			in.limit, n = l, l.Source
		}
		if o, ok := n.(*Order); ok {
			in.order, n = o, o.Source
		}
		if f, ok := n.(*Filter); ok {
			in.filter, n = f, f.Source
		}
		if in.table, ok = n.(*Scan); !ok {
			panic(fmt.Sprintf("engine: a subquery of a node of type %T", n))
		}
		s.list = append(s.list, in)
	}
	return s
}

// tables of the subqueries of a select, whose table is outer: each subquery looks its table up, in
// the order of the text, and fails with SQLite's `no such table` for one that does not exist. Each
// then has its width.
func (s *subqueries) tables(ctx context.Context, r reader, outer *s4qlpb.CreateTable) error {
	if s == nil {
		return nil
	}
	s.outer = outer
	for _, in := range s.list {
		if err := in.find(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// from the table outer of an update or a delete, which its subqueries are correlated with.
func (s *subqueries) from(outer *s4qlpb.CreateTable) {
	if s != nil {
		s.outer = outer
	}
}

// find the table of the subquery in the transaction of r, unless it is found: SQLite's `no such
// table` if it does not exist.
func (in *inner) find(ctx context.Context, r reader) error {
	if in.s != nil {
		return nil
	}
	t, err := r.table(ctx, in.table.Table.Value)
	if err != nil {
		return err
	}
	if t == nil {
		return noSuchTable(in.table.Table.Value)
	}
	in.s = &scan{r: r, table: t}
	in.width = 1
	if in.project.Star {
		in.width = len(t.GetColumns())
	}
	return nil
}

// at gives the subquery that sub names, for a condition of the statement. A condition of a subquery,
// whose outer is not nil, holds none, and an internal error says so.
func (s *subqueries) at(sub *Subquery, outer *s4qlpb.CreateTable) (*inner, error) {
	if s == nil || outer != nil || sub.Number < 1 || sub.Number > len(s.list) {
		return nil, internalError("the subquery %v of a statement with %v", sub.Number, s.count())
	}
	return s.list[sub.Number-1], nil
}

func (s *subqueries) count() int {
	if s == nil {
		return 0
	}
	return len(s.list)
}

// lookup the name, of a condition or an ORDER BY of a table whose columns are of, by its fold: the
// position of its column and true, or false if the table does not have it. For a subquery, outer is
// the table of the statement, and a name that the table does not have and outer has is correlated:
// lookup gives -1 and true, as SQLite takes it, and records the first such name, which
// [subqueries.uncorrelated] refuses.
func (s *subqueries) lookup(of map[string]int, outer *s4qlpb.CreateTable, name stmt.Name) (int, bool) {
	position, ok := of[name.Fold()]
	if ok {
		return position, ok
	}
	if _, ok := columnPositions(outer)[name.Fold()]; !ok {
		return 0, false
	}
	if s.correlated == nil {
		s.correlated = &name
	}
	return -1, true
}

// names of the subquery in, which the walk over the conditions of the statement of the build b has
// reached: it finds the table of the subquery, unless the statement is a select, which has found it,
// and then looks up the column of its select list, the columns of its conditions, as [build.walk]
// does, and those of its ORDER BY, in that order, as SQLite resolves the names of a select, with
// SQLite's `no such column` for the first that neither its table nor the table of the statement has.
func (s *subqueries) names(ctx context.Context, b build, in *inner) error {
	if in.named {
		return internalError("the names of the subquery %v twice", in.number)
	}
	in.named = true
	if err := in.find(ctx, b.r); err != nil {
		return err
	}
	t := in.s.table
	of := columnPositions(t)
	switch {
	case in.project.Star:
	case len(in.project.Columns) != 1:
		return internalError("a subquery of %v columns", len(in.project.Columns))
	default:
		position, ok := s.lookup(of, s.outer, in.project.Columns[0])
		if !ok {
			return noSuchResultColumn(in.project.Columns[0])
		}
		in.position = position
	}
	if in.filter != nil {
		// b is the build of the statement, whose subqueries are s.
		conditions, err := b.walk(ctx, in.filter, t, s.outer)
		if err != nil {
			return err
		}
		in.conditions = conditions
	}
	if in.order != nil {
		columns, err := orderColumns(t, s.outer, in.order, s)
		if err != nil {
			return err
		}
		in.by.columns = columns
	}
	return nil
}

// widths of the subqueries of the conditions of the filter f of an update or a delete, once the
// names of its WHERE are looked up: for a subquery of `*` of a table of more than one column,
// SQLite's `sub-select returns %d columns - expected 1`, or `row value misused` for one on the left of
// != or <>, which SQLite's code generator gives. With two or more such subqueries, which of them
// SQLite names depends on the order in which its plan codes the conditions, so the statement is not
// supported then, at the second.
func (s *subqueries) widths(f *Filter) error {
	// Without subqueries, no condition has one, and s is not read.
	var wide []Condition
	for _, c := range f.Conditions {
		if sub, ok := c.Value.(*Subquery); ok && s.list[sub.Number-1].width > 1 {
			wide = append(wide, c)
		}
	}
	switch len(wide) {
	case 0:
		return nil
	case 1:
		c := wide[0]
		if c.Operator == stmt.OpNotEqual && c.ValueFirst {
			return rowValueMisused(-1)
		}
		return subSelectColumns(s.list[c.Value.(*Subquery).Number-1].width)
	}
	return &stmt.NotSupportedError{What: "second subquery of * of more than one column", Offset: wide[1].Value.(*Subquery).Offset}
}

// uncorrelated refuses a statement with a correlated subquery, once its names are looked up as SQLite
// looks them up, after every error of a name that SQLite gives for it: "not supported", at the first
// correlated name.
func (s *subqueries) uncorrelated() error {
	if s == nil || s.correlated == nil {
		return nil
	}
	return &stmt.NotSupportedError{What: "correlated subquery", Offset: s.correlated.Offset}
}

// lengths of the values of the subquery sub of the statement of the build b, where the statement
// checks the length of the value of its condition: those of its conditions, in the order of the text,
// and then its argument of LIMIT, as a select checks them.
func (s *subqueries) lengths(b build, sub *Subquery) error {
	in, err := s.at(sub, nil)
	if err != nil {
		return err
	}
	if in.filter != nil {
		if err := b.values(in.filter, in.conditions); err != nil {
			return err
		}
	}
	if in.limit != nil {
		if in.count, err = eval(in.limit.Count, b.args); err != nil {
			return err
		}
		if err := checkLength(in.count, b.limit); err != nil {
			return err
		}
	}
	return nil
}

// types of the subquery sub of the statement of the build b, compared with the column at the position
// of the table t of the statement, where the statement checks the type of the value of its
// condition: "not supported" for a value of its conditions of another type than its column, and then
// for an argument of its LIMIT that is not an integer from 0, as a select refuses them; and then for
// its column, if it is of another type than the column that it is compared with.
//
// The column of a subquery is of a table that is STRICT, so every value that it gives is of its type,
// or NULL, and SQLite compares it with the column of the statement by the affinities of the two,
// which convert a value only where the two are of other types. So the rule of exact types holds for
// the two columns, whatever the rows: INT and INTEGER are one type there, and every other two
// types are refused. Where the condition tests an arithmetic expression, test, in place of a
// column, the column of the subquery must be of the class of its values, as [comparable] says.
func (s *subqueries) types(b build, sub *Subquery, t *s4qlpb.CreateTable, position int, test *expr.Bound) error {
	in, err := s.at(sub, nil)
	if err != nil {
		return err
	}
	// Without a filter, there are no conditions, and types checks none.
	if err := b.types(in.filter, in.s, in.conditions); err != nil {
		return err
	}
	if in.limit != nil {
		if in.n, err = limitCount(in.limit.Count, in.count); err != nil {
			return err
		}
	}
	columns := in.s.table.GetColumns()
	if test != nil {
		if in.position < 0 || in.position >= len(columns) {
			return internalError("a subquery of the column at %v compared with an expression", in.position)
		}
		inside := columns[in.position]
		if meetsArithmetic(columnClass(inside.GetType()), arithmeticClass(test)) {
			return nil
		}
		return fmt.Errorf("%w: subquery %v, of the column %v.%v of type %v, compared with %v", stmt.ErrNotSupported,
			in.number, in.s.table.GetTable(), inside.GetName(), typeName(inside.GetType()), expressionName(arithmeticClass(test)))
	}
	if in.position < 0 || in.position >= len(columns) || position < 0 || position >= len(t.GetColumns()) {
		return internalError("a subquery of the column at %v compared with the column at %v", in.position, position)
	}
	inside, outside := columns[in.position], t.GetColumns()[position]
	if storageClass(inside.GetType()) == storageClass(outside.GetType()) {
		return nil
	}
	return fmt.Errorf("%w: subquery %v, of the column %v.%v of type %v, compared with the column %v.%v of type %v", stmt.ErrNotSupported,
		in.number, in.s.table.GetTable(), inside.GetName(), typeName(inside.GetType()), t.GetTable(), outside.GetName(), typeName(outside.GetType()))
}

// storageClass of the type of a column: the kind of the values that a STRICT column of the type
// holds, which is one for INT and INTEGER.
func storageClass(typ s4qlpb.Type) s4qlpb.Type {
	if typ == s4qlpb.Type_TYPE_INT {
		return s4qlpb.Type_TYPE_INTEGER
	}
	return typ
}

// subqueryRead is what a subquery read, for the event of its statement: its access, as
// [report.reads] names an access, the index that it read by, how its rows came in the order of its
// ORDER BY, the rows that it read, and whether it found a row.
type subqueryRead struct {
	access, index, order string
	rows                 int64
	found                bool
}

// compute the value of each subquery of the conditions of the filter f of the scan s, once every
// check of the statement is made, and put it in conditions, in the order of the text. Each subquery
// gets its access by the rules, as a query with a LIMIT, for its conditions and its ORDER BY, and
// reads forward or backward as the engine draws it for a query; it reads its first row, which it
// stops at, and its value is the value of its column in that row, or NULL without a row or with a
// LIMIT of 0, which reads nothing. What each read is recorded on s, for the event of the statement.
//
// A statement that can give or change no row whatever its subqueries give, as SQLite runs no
// subquery for it, computes none, and each value is NULL, which reads nothing more: one whose
// statement says none, with none set, as a query with LIMIT 0 does; one with another condition that
// is never true, as [never] says; and the sink of a write that is built only to look its names up,
// in a [readOnly] transaction, and is not run.
//
// The expressions without a column of the conditions are computed first, as [build.constants] says,
// so that a value of NULL among them makes the statement give no row too.
func (b build) compute(ctx context.Context, f *Filter, s *scan, conditions []condition, none bool) error {
	if err := b.constants(conditions); err != nil {
		return err
	}
	if b.subs == nil {
		return nil
	}
	var others []condition
	for i, c := range f.Conditions {
		if _, ok := c.Value.(*Subquery); !ok {
			others = append(others, conditions[i])
		}
	}
	if _, lookups := b.r.(readOnly); none || lookups || never(s.table, bounds(others)) {
		for i, c := range f.Conditions {
			if _, ok := c.Value.(*Subquery); ok {
				conditions[i].value = null()
			}
		}
		return nil
	}
	var values []*s4qlpb.Value
	// matches are the conditions of the planted bug noRowMatches.
	var at, matches []int
	for i, c := range f.Conditions {
		sub, ok := c.Value.(*Subquery)
		if !ok {
			continue
		}
		in, err := b.subs.at(sub, nil)
		if err != nil {
			return err
		}
		v, read, err := b.value(ctx, in)
		if err != nil {
			return err
		}
		if b.how.bugs.keepValue && b.how.e != nil {
			// The planted bug: the value of the last run of a statement whose plan prints alike.
			key := fmt.Sprint(in.number, "\n", Format(in.query))
			if kept, ok := b.how.e.kept.Load(key); ok {
				v = kept.(*s4qlpb.Value)
			}
			b.how.e.kept.Store(key, v)
		}
		if b.how.bugs.noRowMatches && !read.found {
			matches = append(matches, i)
		}
		s.subqueries = append(s.subqueries, read)
		values, at = append(values, v), append(at, i)
	}
	if b.how.bugs.swapSubqueries && len(values) > 1 {
		// The planted bug: the first subquery gets the value of the second, and the second that of the
		// first.
		values[0], values[1] = values[1], values[0]
	}
	if b.how.observe != nil {
		// The hook of the tests, which may give other values.
		values = b.how.observe(ctx, values)
	}
	for j, i := range at {
		conditions[i].value = values[j]
	}
	for _, i := range matches {
		// The planted bug: no row makes the comparison true of every row whose column is not NULL.
		conditions[i].op, conditions[i].value = stmt.OpIsNotNull, nil
	}
	return nil
}

// value of the subquery in, and what it read.
func (b build) value(ctx context.Context, in *inner) (*s4qlpb.Value, subqueryRead, error) {
	if in.limit != nil && in.n == 0 && !b.how.bugs.limitZeroRow {
		// LIMIT 0 gives no row, and reads none; the planted bug limitZeroRow reads the first.
		return null(), subqueryRead{access: "none"}, nil
	}
	// The values of the subquery are evaluated and checked already, so the build of its access needs
	// neither the arguments nor the length limit.
	// The expressions without a column of its conditions are computed when the subquery is.
	if err := b.constants(in.conditions); err != nil {
		return nil, subqueryRead{}, err
	}
	inside := build{r: b.r, how: b.how.inner(), order: in.by}
	if b.how.bugs.orderIgnored {
		// The planted bug: the access of the conditions alone, whose first row is taken.
		inside.order = orderBy{}
	}
	inside.order.limited = true
	inside.order.turnMixed, inside.order.ascendLast, inside.order.skipGap = inside.how.bugs.turnMixed, inside.how.bugs.ascendLast, inside.how.bugs.skipGap
	if in.filter != nil || in.order != nil {
		if err := inside.reach(ctx, in.s, in.conditions); err != nil {
			return nil, subqueryRead{}, err
		}
	} else {
		in.s.access.backward = inside.how.backward
	}
	var src source = in.s
	if b.how.outside != nil {
		// The planted bug outsideTx: the scan reads the newest state that is committed.
		outside := *in.s
		outside.r = b.how.outside
		src = &outside
	}
	if in.filter != nil {
		src = &filter{src: src, conditions: in.conditions}
	}
	read := readOf(in.s.access)
	before := b.r.rowsRead()
	v := null()
	for row, err := range src.rows(ctx) {
		if err != nil {
			return nil, subqueryRead{}, err
		}
		if in.position < 0 || in.position >= len(row) {
			return nil, subqueryRead{}, internalError("a subquery of the column at %v of a row of %v values", in.position, len(row))
		}
		if b.how.bugs.firstNotNull && row[in.position].HasNull() {
			// The planted bug: a row whose value is NULL is passed over.
			continue
		}
		v, read.found = row[in.position], true
		if !b.how.bugs.lastRow {
			break
		}
		// Else the planted bug: the rows after the first are read too, and the last gives the value.
	}
	read.rows = b.r.rowsRead() - before
	return v, read, nil
}

// readOf the access a, as the event of a statement names it: its access, its index, and its order.
func readOf(a access) subqueryRead {
	var r subqueryRead
	switch {
	case a.none:
		r.access = "none"
	case a.full():
		r.access = "full_scan"
	case a.index != "":
		r.access, r.index = "index", a.index
	default:
		r.access = "primary_key"
	}
	switch a.order {
	case delivered:
		r.order = "delivered"
	case sorted:
		r.order = "sorted"
	}
	return r
}

// rowValueMisused is SQLite's `row value misused`, of a subquery of more than one column compared
// with a column, at the offset of the left side of the comparison, or -1 where SQLite gives none.
func rowValueMisused(offset int) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: "row value misused", Offset: offset}
}

// subSelectColumns is SQLite's `sub-select returns %d columns - expected 1`, of a subquery of n
// columns compared with a column in an update or a delete.
func subSelectColumns(n int) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: fmt.Sprintf("sub-select returns %v columns - expected 1", n), Offset: -1}
}
