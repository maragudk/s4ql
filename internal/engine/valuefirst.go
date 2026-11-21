package engine

import (
	"maragu.dev/s4ql/internal/sql/stmt"
)

// valueFirstRefusal of the statement s: for a select, an update, or a delete with a comparison that
// has the value on the left, of slice 12, which the pass supports and the engine does not run yet,
// the "not supported" that the pass gave such a statement before it supported it, and nil for any
// other statement. The pass named the first such comparison at the start of its value, its sign
// included. A text that the pass refuses for another token after the value, such as a hex literal
// in a later condition, gets the pass's error for that token, which it did not get before. The pass
// gives the name no more, so the vocabulary in the README of the pass does not list it.
func valueFirstRefusal(s stmt.Statement) error {
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
		if c.ValueFirst {
			return &stmt.NotSupportedError{What: "value on the left of a comparison", Offset: c.Value.Span.Start}
		}
	}
	return nil
}
