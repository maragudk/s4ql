package engine

import (
	"maragu.dev/s4ql/internal/sql/stmt"
)

// subqueryRefusal of the statement s: for a select, an update, or a delete with a scalar subquery in
// its WHERE, of slice 15, which the pass supports and the engine does not run yet, the "not
// supported" that the pass gave such a statement before it supported it, `subquery` at the opening
// parenthesis of its first subquery; and nil for any other statement. A text that the pass refuses
// for a token inside a subquery, such as `(select v + 1 from t)`, gets the pass's error for that
// token, which it did not get before.
func subqueryRefusal(s stmt.Statement) error {
	var where []stmt.Condition
	switch s := s.(type) {
	case *stmt.Select:
		where = s.Where
	case *stmt.Update:
		where = s.Where
	case *stmt.Delete:
		where = s.Where
	}
	for _, c := range where {
		// The first subquery in the order of the text has the number 1.
		if c.Value.Subquery == 1 {
			return &stmt.NotSupportedError{What: "subquery", Offset: c.Value.Span.Start}
		}
	}
	return nil
}
