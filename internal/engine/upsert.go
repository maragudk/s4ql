package engine

import (
	"maragu.dev/s4ql/internal/sql/stmt"
)

// upsertRefusal of the statement s: for an insert with an ON CONFLICT clause, which the engine does
// not run yet, the "not supported" of the clause, at its ON; and nil for any other statement. It
// reads only the statement, so its answer does not depend on what the database holds.
func upsertRefusal(s stmt.Statement) error {
	if i, ok := s.(*stmt.Insert); ok && i.Conflict.Present() {
		return &stmt.NotSupportedError{What: "ON CONFLICT clause", Offset: i.Conflict.Span.Start}
	}
	return nil
}
