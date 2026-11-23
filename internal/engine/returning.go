package engine

import (
	"maragu.dev/s4ql/internal/sql/stmt"
)

// returningRefusal of the statement s: for an insert, an update, or a delete with RETURNING, of slice
// 14, which the pass supports and the engine does not run yet, the "not supported" that the pass gave
// such a statement before it supported it, `RETURNING clause` at the keyword RETURNING; and nil for
// any other statement. A text that the pass refuses for a token in RETURNING, such as `returning v +
// 1`, gets the pass's error for that token, which it did not get before.
func returningRefusal(s stmt.Statement) error {
	var r stmt.Returning
	switch s := s.(type) {
	case *stmt.Insert:
		r = s.Returning
	case *stmt.Update:
		r = s.Returning
	case *stmt.Delete:
		r = s.Returning
	}
	if !r.Present() {
		return nil
	}
	return &stmt.NotSupportedError{What: "RETURNING clause", Offset: r.Span.Start}
}
