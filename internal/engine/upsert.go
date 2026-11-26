package engine

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// The upserts: `insert ... on conflict (key) do nothing` and `insert ... on conflict (key) do update
// set ... [where ...]`, of one row. The insert proposes its row as an insert does; where the table
// has no row of its key, the upsert inserts it, and otherwise it does what its clause says.
//
// DO UPDATE reads two rows: the row that the table holds, whose columns it names as an update does,
// and the row that the insert proposes, whose columns it names as excluded.c. The engine reads them as
// one row of a combined table, [combinedTable]: the columns of the table, and after them a column for
// each of them under the name that [excludedName] gives, which no column of a table has. Each
// excluded.c of the clause becomes a column of that table, [combinedExpression], once SQLite would
// have given every error of its names, so that DO UPDATE is checked, bound, and computed by the
// operators of an update and a filter, as for a column of the type of its column: by the rule of
// exact types, a copy of excluded.c alone, as a column alone is copied, and arithmetic with it.

// excludedPrefix is the start of the name of a column of the row that an upsert proposes, in its
// combined table: a NUL byte, which the text of no statement holds, so that the name is no name of a
// column of a table.
const excludedPrefix = "\x00"

// excludedName of the column of the name in the combined table of an upsert, for the column of the
// name of the row that the insert proposes.
func excludedName(name string) string {
	return excludedPrefix + name
}

// combinedTable of an upsert into the table t: a table of the name of t, of its columns in their
// order, and then of a column of the name that [excludedName] gives and of the type of each, in their
// order, with the key of t. A row of it is a row of t followed by the row that the insert proposes.
func combinedTable(t *s4qlpb.CreateTable) *s4qlpb.CreateTable {
	columns := slices.Clone(t.GetColumns())
	for _, c := range t.GetColumns() {
		columns = append(columns, s4qlpb.Column_builder{Name: new(excludedName(c.GetName())), Type: new(c.GetType())}.Build())
	}
	return s4qlpb.CreateTable_builder{Table: new(t.GetTable()), Columns: columns, PrimaryKey: t.GetPrimaryKey()}.Build()
}

// combinedExpression of the expression e of DO UPDATE: e with each excluded.c as the column of its
// name in the combined table, at the offset of excluded.c.
func combinedExpression(e stmt.Expression) stmt.Expression {
	if e.Kind == stmt.ExprExcluded {
		name := excludedName(e.Column.Value)
		e.Kind, e.Column, e.Table = stmt.ExprColumn, stmt.Name{Value: name, Raw: name, Offset: e.Table.Offset}, stmt.Name{}
		return e
	}
	if len(e.Operands) == 0 {
		return e
	}
	operands := make([]stmt.Expression, len(e.Operands))
	for i, o := range e.Operands {
		operands[i] = combinedExpression(o)
	}
	e.Operands = operands
	return e
}

// combinedValue of a value of DO UPDATE: its expression as [combinedExpression] gives it, or the
// value itself if it is no expression.
func combinedValue(v Expr) Expr {
	if a, ok := v.(*Arithmetic); ok {
		return &Arithmetic{Expression: combinedExpression(a.Expression)}
	}
	return v
}

// combinedConditions of the WHERE of DO UPDATE: each condition with its expressions as
// [combinedExpression] gives them, and a condition that tests excluded.c alone as one that tests the
// column of the combined table.
func combinedConditions(where []Condition) []Condition {
	var conditions []Condition
	for _, c := range where {
		if c.Value != nil {
			c.Value = combinedValue(c.Value)
		}
		if c.Expression.Present() {
			e := combinedExpression(c.Expression)
			c.Expression = e
			if e.Kind == stmt.ExprColumn {
				c.Column, c.Expression = e.Column, stmt.Expression{}
			}
		}
		conditions = append(conditions, c)
	}
	return conditions
}

// conflictNames of the clause c of an upsert into the table t, whose primary key is the column at the
// position key: SQLite's errors of its names, in SQLite's order, or nil. First come the columns of
// the target, in their order, with `no such column` for one that the table does not have, and then
// `ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint` for a target that is not
// the key. Then come the assignments of SET, in the order of the text, each its value before its
// column, as for an update, the value a walk of its own, and then the conditions of WHERE, one walk,
// as [whereNames] says. The walks are those of [resolver], with a column of excluded as a column, so
// one that the table does not have is `no such column: excluded.c`.
func conflictNames(t *s4qlpb.CreateTable, key int, c *Conflict) error {
	of := columnPositions(t)
	var target []int
	for _, name := range c.Target {
		position, ok := of[name.Fold()]
		if !ok {
			return noSuchResultColumn(name)
		}
		target = append(target, position)
	}
	if len(target) != 1 || target[0] != key {
		return conflictTarget()
	}
	found := func(name stmt.Name) bool { _, ok := of[name.Fold()]; return ok }
	for _, a := range c.Set {
		if arithmetic, ok := a.Value.(*Arithmetic); ok {
			n := &resolver{found: found}
			if name, missing := n.all(arithmetic.Expression); missing {
				return noSuchResultColumn(name)
			}
		}
		if _, ok := of[a.Column.Fold()]; !ok {
			return noSuchSetColumn(a.Column)
		}
	}
	return whereNames(c.Where, found)
}

// whereNames of the conditions of the WHERE of DO UPDATE, for whose names found reports whether the
// table has a column of the name: SQLite's error of their names, or nil. SQLite looks them up in one
// walk, as it does those of the WHERE of a select, as [build.walk] says: the names of a comparison in
// the order of the text, where a missing column ends the walk, but where [resolver] says that it is
// pending; a test for NULL of a column that the table does not have, or of excluded.c of one, is
// pending too, until the next comparison or the end, unless a later missing column replaces it; and a
// literal or a parameter after the tested side ends the walk with a pending column.
func whereNames(where []Condition, found func(stmt.Name) bool) error {
	n := &resolver{found: found}
	tested := func(c Condition) (stmt.Name, bool) {
		if c.Expression.Present() {
			return n.resolve(c.Expression)
		}
		if !found(c.Column) {
			return c.Column, true
		}
		return stmt.Name{}, false
	}
	for _, c := range where {
		if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull {
			if name, missing := tested(c); missing {
				n.pending = &name
			}
			continue
		}
		if n.pending != nil {
			return noSuchResultColumn(*n.pending)
		}
		value, _ := c.Value.(*Arithmetic)
		if value != nil && c.ValueFirst {
			if name, stop := n.resolve(value.Expression); stop {
				return noSuchResultColumn(name)
			}
		}
		if name, stop := tested(c); stop {
			return noSuchResultColumn(name)
		}
		if value != nil && !c.ValueFirst {
			if name, stop := n.resolve(value.Expression); stop {
				return noSuchResultColumn(name)
			}
		}
		if value == nil && !c.ValueFirst && n.pending != nil {
			return noSuchResultColumn(*n.pending)
		}
	}
	if n.pending != nil {
		return noSuchResultColumn(*n.pending)
	}
	return nil
}

// doUpdate of the clause c of an upsert into the table t, whose names [conflictNames] has checked, in
// the transaction of w: the [update] that sets the columns of SET in a row of the combined table of
// t, and the [filter] of the conditions of WHERE on such a row, or nil if c has none. It fails with
// SQLite's error for the first value of SET, and then of WHERE, in the order of the text, that is
// longer than the limit of b, as an update gives it; and then, in a write transaction, with "not
// supported" for the values of another type than what they meet, as an update does, SET before
// WHERE, and with the evaluation error of an expression without a column, which it computes once,
// SET before WHERE, as an update computes them, and with SQLite's `string or blob too big` for a text
// that SQLite computes at the start of the statement, where the table has a row of the key or not:
// the subtrees that [hoisted] gives and the sides of a comparison without a column and without a
// call. A text without a column that is too long in SET, and a side with a call, fail on the row of
// the conflict, where SQLite computes them. In a [readOnly] transaction, SQLite refuses the write
// before it looks at a value, so the expressions are neither checked nor computed there, and the
// update and the filter are built only for their lengths.
func (b build) doUpdate(ctx context.Context, w writer, t *s4qlpb.CreateTable, c *Conflict) (*update, *filter, error) {
	both := combinedTable(t)
	of := columnPositions(both)
	key := int(t.GetPrimaryKey()[0])
	set := make([]Assignment, len(c.Set))
	u := &update{w: w, table: both, key: key, positions: make([]int, len(c.Set)), values: make([]*s4qlpb.Value, len(c.Set)), exprs: make([]Expr, len(c.Set)),
		computed: make([]*expr.Bound, len(c.Set)), copied: make([]int, len(c.Set))}
	for i, a := range c.Set {
		set[i] = Assignment{Column: a.Column, Value: combinedValue(a.Value)}
		position, ok := of[a.Column.Fold()]
		if !ok || position >= len(t.GetColumns()) {
			return nil, nil, internalError("an assignment of DO UPDATE to the column %v, which the table %v does not have", a.Column.Value, t.GetTable())
		}
		if slices.Contains(u.positions[:i], position) {
			return nil, nil, internalError("a DO UPDATE that sets the column %v twice", a.Column.Value)
		}
		u.positions[i], u.exprs[i], u.copied[i] = position, set[i].Value, -1
		if arithmetic, ok := set[i].Value.(*Arithmetic); ok {
			// An expression is computed later, but the arguments of its parameters are checked here.
			if err := argumentLengths(arithmetic.Expression, b.args, b.limit); err != nil {
				return nil, nil, err
			}
			continue
		}
		v, err := eval(set[i].Value, b.args)
		if err != nil {
			return nil, nil, err
		}
		if err := checkLength(v, b.limit); err != nil {
			return nil, nil, err
		}
		u.values[i] = v
	}
	u.order = make([]int, len(u.positions))
	for i := range u.order {
		u.order[i] = i
	}
	slices.SortFunc(u.order, func(a, b int) int { return cmp.Compare(u.positions[a], u.positions[b]) })

	var f *Filter
	var conditions []condition
	s := &scan{table: both}
	if c.Where != nil {
		f = &Filter{Source: &Scan{Table: stmt.Name{Value: t.GetTable()}}, Conditions: combinedConditions(c.Where)}
		var err error
		if conditions, err = b.walk(ctx, f, both, nil); err != nil {
			return nil, nil, err
		}
		if err := b.values(f, conditions); err != nil {
			return nil, nil, err
		}
	}
	where := &filter{src: s, conditions: conditions}
	if f == nil {
		where = nil
	}
	if _, lookups := w.(readOnly); lookups {
		return u, where, nil
	}

	if err := u.bind(of, set, b.args, b.limit); err != nil {
		return nil, nil, err
	}
	if err := u.checkTypes(); err != nil {
		return nil, nil, err
	}
	if f != nil {
		if err := b.types(f, s, conditions); err != nil {
			return nil, nil, err
		}
	}
	// As for an update: the subtrees of SET that SQLite computes before any row, and then the values of
	// SET without a column, but one of text that is too long, which SQLite computes on the row that
	// DO UPDATE changes, and the update computes there too. SQLite computes them at the start of the
	// statement, also where the table has no row of the key.
	if err := computeHoisted(u.hoisted); err != nil {
		return nil, nil, err
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
			return nil, nil, fromExpr(err)
		}
		u.values[i] = v.Proto()
	}
	// Then the subtrees and the constants of WHERE. SQLite computes WHERE on the row of the conflict
	// and never to find a row, so a constant of text with a call that is too long serves no access,
	// whatever the key and the indexes, and fails on the row, as [build.constants] says for the nil
	// table.
	if err := b.hoist(ctx, both, conditions, false); err != nil {
		return nil, nil, err
	}
	if err := b.constants(ctx, nil, conditions); err != nil {
		return nil, nil, err
	}
	return u, where, nil
}

// upsert is the sink of an insert with ON CONFLICT, of one row into a table whose primary key is one
// column: the insert of its row, which it checks as an [insert] does, where the table has no row of
// its key, and otherwise nothing for DO NOTHING, or, for DO UPDATE, the change of the row of the key
// that the table holds, if the conditions of where hold for it, with the values of set. A row whose
// record is too long gets `string or blob too big` before the upsert looks for its key, as SQLite
// builds the record first, also where the table has a row of the key, and so does the row that DO
// UPDATE changes, when the upsert writes it. Its RETURNING gives the row that it inserts, or the row
// that it changes, with the values after the change, and none where it changes nothing.
type upsert struct {
	insert
	// nothing reports whether the clause says DO NOTHING.
	nothing bool
	// set holds the assignments of DO UPDATE, as an [update] holds them, of the row of the combined
	// table of the upsert, [combinedTable], whose values are those of the row that the table holds
	// followed by those of the row that the insert proposes. It is nil for DO NOTHING.
	set *update
	// where holds the conditions of the WHERE of DO UPDATE on such a row, or is nil without a WHERE.
	where *filter
}

func (u *upsert) run(ctx context.Context) (int64, error) {
	if len(u.src.data) != 1 {
		return 0, internalError("an upsert of %v rows", len(u.src.data))
	}
	name, row := u.table.GetTable(), u.src.data[0]
	inserted, err := u.w.insertNew(ctx, name, row)
	if err != nil {
		return 0, err
	}
	if inserted {
		u.ret.add(row)
		return 1, nil
	}
	if u.nothing {
		return 0, nil
	}
	old, err := u.w.get(ctx, name, []*s4qlpb.Value{row[u.key]})
	if err != nil {
		return 0, err
	}
	if old == nil {
		return 0, internalError("the row of a key that an upsert did not insert is gone")
	}
	both := slices.Concat(old, row)
	if u.where != nil {
		holds, err := u.where.holds(both)
		if err != nil || !holds {
			return 0, err
		}
	}
	changed, err := u.set.newRow(both)
	if err != nil {
		return 0, err
	}
	changed = changed[:len(old)]
	// SQLite checks NOT NULL on the row that DO UPDATE changes, for a value of one run of the statement
	// too, in the order of the table.
	columns := u.table.GetColumns()
	for _, i := range u.set.order {
		position := u.set.positions[i]
		if changed[position].HasNull() && (position == u.key || !columns[position].GetNullable()) {
			return 0, notNullFailed(name, columns[position].GetName())
		}
	}
	if err := u.w.upsert(ctx, name, changed); err != nil {
		return 0, err
	}
	u.ret.add(changed)
	return 1, nil
}
