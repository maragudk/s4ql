package engine

import (
	"cmp"
	"context"
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
// column, as for an update, and then the conditions of WHERE, as [whereNames] says. A column of
// excluded that the table does not have is `no such column: excluded.c`, as [noSuchExcluded] gives
// it.
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
	for _, a := range c.Set {
		if arithmetic, ok := a.Value.(*Arithmetic); ok {
			if err := missingNames(arithmetic.Expression, of); err != nil {
				return err
			}
		}
		if _, ok := of[a.Column.Fold()]; !ok {
			return noSuchSetColumn(a.Column)
		}
	}
	return whereNames(c.Where, of)
}

// missingNames of the expression e of DO UPDATE, whose table has the columns of: SQLite's error for
// the first column, or column of excluded, in the order of the text, that the table does not have,
// or nil.
func missingNames(e stmt.Expression, of map[string]int) error {
	switch e.Kind {
	case stmt.ExprColumn:
		if _, ok := of[e.Column.Fold()]; !ok {
			return noSuchResultColumn(e.Column)
		}
	case stmt.ExprExcluded:
		if _, ok := of[e.Column.Fold()]; !ok {
			return noSuchExcluded(e)
		}
	}
	for _, o := range e.Operands {
		if err := missingNames(o, of); err != nil {
			return err
		}
	}
	return nil
}

// whereNames of the conditions of the WHERE of DO UPDATE, whose table has the columns of: SQLite's
// error of their names, or nil. SQLite looks them up as it does those of a WHERE of a select, as
// [build.walk] says: the names of a comparison in the order of the text, where the first that the
// table does not have fails; and a test for NULL of a column that the table does not have, or of
// excluded.c of such a column, does not stop it, but gives its error at the next comparison, or at
// the end, unless a later test for NULL replaces it.
func whereNames(where []Condition, of map[string]int) error {
	var missing error
	tested := func(c Condition) error {
		if c.Expression.Present() {
			return missingNames(c.Expression, of)
		}
		if _, ok := of[c.Column.Fold()]; !ok {
			return noSuchResultColumn(c.Column)
		}
		return nil
	}
	for _, c := range where {
		if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull {
			if err := tested(c); err != nil {
				missing = err
			}
			continue
		}
		if missing != nil {
			return missing
		}
		value, _ := c.Value.(*Arithmetic)
		if value != nil && c.ValueFirst {
			if err := missingNames(value.Expression, of); err != nil {
				return err
			}
		}
		if err := tested(c); err != nil {
			return err
		}
		if value != nil && !c.ValueFirst {
			if err := missingNames(value.Expression, of); err != nil {
				return err
			}
		}
	}
	return missing
}

// doUpdate of the clause c of an upsert into the table t, whose names [conflictNames] has checked, in
// the transaction of w: the [update] that sets the columns of SET in a row of the combined table of
// t, and the [filter] of the conditions of WHERE on such a row, or nil if c has none. It fails with
// SQLite's error for the first value of SET, and then of WHERE, in the order of the text, that is
// longer than the limit of b, as an update gives it; and then, in a write transaction, with "not
// supported" for the values of another type than what they meet, as an update does, SET before
// WHERE, and with the evaluation error of an expression without a column, which it computes once,
// SET before WHERE, as an update computes them. In a [readOnly] transaction, SQLite refuses the write
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
			// A number has no length, but an argument of its parameters has.
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

	if err := u.bind(of, set, b.args); err != nil {
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
	for i, bound := range u.computed {
		if bound == nil || set[i].Value.(*Arithmetic).Expression.HasColumn() {
			continue
		}
		v, err := bound.Eval(nil)
		if err != nil {
			return nil, nil, fromExpr(err)
		}
		u.values[i] = v.Proto()
	}
	if err := b.constants(conditions); err != nil {
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
