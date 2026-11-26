package stmttest

import (
	"fmt"
	"slices"
	"strings"

	"maragu.dev/s4ql/internal/sql/stmt"
)

// The rules of the upserts, as s4ql's documents give them: DO UPDATE reads the row that the table
// holds and the row that the insert proposes, and excluded.c is a column of the type of its column,
// copied and compared as a column alone is, and in arithmetic and in text as a column is. The rules read DO
// UPDATE as SET and WHERE over the columns of the table followed by a column for each of them under
// the name that [excludedColumn] gives, so that the rules of an update give its refusals.

// excludedPrefix starts the name of a column of the row that an upsert proposes, among the columns
// that the rules read DO UPDATE over: a NUL byte, which no name of a statement holds.
const excludedPrefix = "\x00"

// excludedColumn of the name: the name of the column of the row that an upsert proposes.
func excludedColumn(name string) string {
	return excludedPrefix + name
}

// UpsertRefusals of the upsert s, an insert with ON CONFLICT, into the table of the name, as the
// table stores it, and of the columns, in the order of the table, which SQLite prepares, with the
// arguments args as SQLite binds them, one for each parameter: every "not supported" that s4ql's
// rules give for it, whatever their order. They are none in a read-only transaction, which refuses
// the write before s4ql looks at a value; otherwise the first value of the proposed row, in the order
// of the table, of another type than its column, as for an insert, and the refusals of the SET and
// the WHERE of DO UPDATE, as [ArithmeticRefusals] gives those of an update, of arithmetic and of
// text, with excluded.c as a column of its type, named "the column excluded.c of type T" in a
// message.
func UpsertRefusals(table string, columns []TableColumn, s *stmt.Insert, args []any, readOnly bool) []string {
	if readOnly {
		return nil
	}
	var refusals []string
	if positions, ok := Positions(columns, s); ok {
		for position, i := range valuesOf(columns, positions) {
			if i < 0 {
				continue
			}
			if kind, what := ValueKind(s.Values[i], args); kind != "null" && columnHolds[columns[position].Type] != kind {
				refusals = append(refusals, fmt.Sprintf("s4ql: not supported: %v for the column %v.%v of type %v", what, table, columns[position].Name, columns[position].Type))
				break
			}
		}
	}
	both := upsertColumns(columns)
	set, where := UpsertClause(s.Conflict)
	refusals = append(refusals, setRefusals(table, both, set, args)...)
	return append(refusals, whereRefusals(table, both, where, nil, args, nil)...)
}

// upsertColumns of DO UPDATE of a table of the columns: the columns, and then a column of each, of
// its type, under the name that [excludedColumn] gives.
func upsertColumns(columns []TableColumn) []TableColumn {
	both := append([]TableColumn(nil), columns...)
	for _, c := range columns {
		both = append(both, TableColumn{Name: excludedColumn(c.Name), Type: c.Type, Nullable: true})
	}
	return both
}

// UpsertClause of the clause c of an upsert: its SET and its WHERE with each excluded.c as the column
// that [excludedColumn] names, and a condition that tests excluded.c alone as one that tests that
// column, as the rules read them.
func UpsertClause(c stmt.Conflict) ([]stmt.Assignment, []stmt.Condition) {
	var set []stmt.Assignment
	for _, a := range c.Set {
		a.Value.Expression = upsertExpression(a.Value.Expression)
		set = append(set, a)
	}
	var where []stmt.Condition
	for _, cond := range c.Where {
		cond.Value.Expression = upsertExpression(cond.Value.Expression)
		if e := upsertExpression(cond.Expression); e.Kind == stmt.ExprColumn {
			cond.Column, cond.Expression = e.Column, stmt.Expression{}
		} else {
			cond.Expression = e
		}
		where = append(where, cond)
	}
	return set, where
}

// upsertExpression of the expression e of DO UPDATE: e with each excluded.c as the column that
// [excludedColumn] names, at the offset of excluded.c.
func upsertExpression(e stmt.Expression) stmt.Expression {
	if e.Kind == stmt.ExprExcluded {
		name := excludedColumn(e.Column.Value)
		e.Kind, e.Column, e.Table = stmt.ExprColumn, stmt.Name{Value: name, Raw: name, Offset: e.Table.Offset}, stmt.Name{}
		return e
	}
	if len(e.Operands) == 0 {
		return e
	}
	operands := make([]stmt.Expression, len(e.Operands))
	for i, o := range e.Operands {
		operands[i] = upsertExpression(o)
	}
	e.Operands = operands
	return e
}

// excludedName of a column of the columns that the rules read DO UPDATE over: the name of the column
// of excluded without its prefix, and true, or the name and false for a column of the table.
func excludedName(name string) (string, bool) {
	return strings.CutPrefix(name, excludedPrefix)
}

// UpsertSites of the clause c of an upsert, in the order of the text: SET, at the index Set, and the
// WHERE of DO UPDATE, at the index Where, as [ArithmeticSites] gives those of an update, but a column
// alone and excluded.c alone, which are copied or compared without the evaluator.
func UpsertSites(c stmt.Conflict) []ArithmeticSite {
	var sites []ArithmeticSite
	add := func(e stmt.Expression, site ArithmeticSite) {
		if e.Present() && e.Kind != stmt.ExprColumn && e.Kind != stmt.ExprExcluded {
			site.Root = e
			sites = append(sites, site)
		}
	}
	for i, a := range c.Set {
		add(a.Value.Expression, ArithmeticSite{Set: i, Where: -1})
	}
	for j, cond := range c.Where {
		add(cond.Expression, ArithmeticSite{Set: -1, Where: j})
		add(cond.Value.Expression, ArithmeticSite{Set: -1, Where: j})
	}
	return sites
}

// UpsertQuirkQuery of the operator n of the site of DO UPDATE of the upsert s, of the text src, into
// a table whose key is the column of the name key, of the kind, with the arguments args: a statement
// that gives a row where SQLite shows the quirk of n, as [Quirk] says, and its arguments. For a site
// whose root names no column, of the table or of excluded, it is a select of the one value of n,
// which needs no row. Otherwise it is the upsert of the row of s, with a DO UPDATE that sets the key
// to itself where every condition holds that the documented rule evaluates the site after, those of
// the WHERE before the site, or every one for SET, and the quirk shows, and with RETURNING 1: it gives
// a row where the table has a row of the key of the proposed row for which the rule evaluates n and
// SQLite shows the quirk, and the row that it inserts where the table has none of the key, which the
// caller tells apart by [UpsertConflictQuery]. It changes no value, but it writes, so the caller runs
// it in a savepoint that it rolls back.
func UpsertQuirkQuery(s *stmt.Insert, src, key string, site ArithmeticSite, n stmt.Expression, kind error, args []any) (string, []any) {
	condition, bound := Quirk(n, kind, args)
	if !site.Root.HasColumn() {
		return "select 1 where " + condition, bound
	}
	guard := s.Conflict.Where
	if site.Set < 0 {
		guard = guard[:site.Where]
	}
	where, whereArgs := whereText(guard, nil, args)
	if where == "" {
		where = " where "
	} else {
		where += " and "
	}
	text, valueArgs := UpsertConflictQuery(s, src, key, args)
	text = strings.TrimSuffix(text, "do nothing") + "do update set " + Quote(key) + " = " + Quote(key) + where + condition + " returning 1"
	return text, slices.Concat(valueArgs, whereArgs, bound)
}

// UpsertConflictQuery of the upsert s of the text src, into a table whose key is the column of the
// name key, with the arguments args: the insert of the row of s with DO NOTHING, and its arguments,
// which changes a row, as SQLite's count of changes says, where the table has no row of the key of
// the proposed row, and none where it has one. The caller runs it in a savepoint that it rolls back.
func UpsertConflictQuery(s *stmt.Insert, src, key string, args []any) (string, []any) {
	var valueArgs []any
	for _, v := range s.Values {
		if v.Parameter > 0 {
			valueArgs = append(valueArgs, args[v.Parameter-1])
		}
	}
	return src[s.Span.Start:s.Conflict.Span.Start] + "on conflict (" + Quote(key) + ") do nothing", valueArgs
}
