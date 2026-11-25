package engine

import (
	"maragu.dev/s4ql/internal/sql/stmt"
)

// expressionRefusal of the statement s: for an update with an arithmetic expression in its SET, or a
// select, an update, or a delete with one in a condition of its WHERE or of a subquery, which the
// pass supports and the engine does not evaluate yet, `s4ql: not supported: expression` at the start
// of the first expression in the order of the text; and nil for any other statement.
func expressionRefusal(s stmt.Statement) error {
	var where []stmt.Condition
	var subqueries []stmt.Select
	first := -1
	at := func(t stmt.Term) {
		if t.Expression.Present() && (first < 0 || t.Span.Start < first) {
			first = t.Span.Start
		}
	}
	switch s := s.(type) {
	case *stmt.Select:
		where, subqueries = s.Where, s.Subqueries
	case *stmt.Update:
		where, subqueries = s.Where, s.Subqueries
		for _, a := range s.Set {
			at(a.Value)
		}
	case *stmt.Delete:
		where, subqueries = s.Where, s.Subqueries
	}
	for _, q := range subqueries {
		where = append(where[:len(where):len(where)], q.Where...)
	}
	for _, c := range where {
		at(c.Value)
		if c.Expression.Present() {
			at(stmt.Term{Expression: c.Expression, Span: c.Expression.Span})
		}
	}
	if first < 0 {
		return nil
	}
	return &stmt.NotSupportedError{What: "expression", Offset: first}
}
