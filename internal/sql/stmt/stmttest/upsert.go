package stmttest

import (
	"fmt"

	"maragu.dev/s4ql/internal/sql/stmt"
)

// UpsertRefusal of the statement s: for an insert with an ON CONFLICT clause, s4ql's "not supported"
// of the clause, at its ON, which s4ql gives for the whole statement whatever the database holds,
// once the number of its arguments is right, as `ON CONFLICT clause`; or "" for any other statement.
// The engine does not run an upsert yet.
func UpsertRefusal(s stmt.Statement) string {
	if i, ok := s.(*stmt.Insert); ok && i.Conflict.Present() {
		return fmt.Sprintf("s4ql: not supported: ON CONFLICT clause (at offset %v)", i.Conflict.Span.Start)
	}
	return ""
}
