package engine_test

import (
	"maragu.dev/s4ql/internal/sql/stmt"
)

// expressed reports whether s is a statement with an arithmetic expression: in the SET of an update,
// or in a condition of its WHERE or of the WHERE of a subquery.
func expressed(s stmt.Statement) bool {
	var where []stmt.Condition
	var subqueries []stmt.Select
	switch s := s.(type) {
	case *stmt.Select:
		where, subqueries = s.Where, s.Subqueries
	case *stmt.Update:
		where, subqueries = s.Where, s.Subqueries
		for _, a := range s.Set {
			if a.Value.Expression.Present() {
				return true
			}
		}
	case *stmt.Delete:
		where, subqueries = s.Where, s.Subqueries
	}
	for _, q := range subqueries {
		where = append(where[:len(where):len(where)], q.Where...)
	}
	for _, c := range where {
		if c.Expression.Present() || c.Value.Expression.Present() {
			return true
		}
	}
	return false
}
