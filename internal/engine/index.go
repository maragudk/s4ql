package engine

import (
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/token"
)

// indexRefusal of the statement s of the text src: for a create index and a drop index of slice 9,
// which the pass supports and the engine does not run yet, the "not supported" that the pass gave
// them before it supported them, and nil for a statement of another kind. The pass named a create
// index at its CREATE, and a drop index at its INDEX.
func indexRefusal(src string, s stmt.Statement) error {
	switch s := s.(type) {
	case *stmt.CreateIndex:
		return &stmt.NotSupportedError{What: "CREATE INDEX", Offset: s.Span.Start}
	case *stmt.DropIndex:
		// The first token of the statement is its DROP, and the next that is not a space or a comment
		// its INDEX.
		at := s.Span.Start
		_, n := token.Next(src[at:])
		at += n
		for {
			kind, n := token.Next(src[at:])
			if kind != token.Space && kind != token.Comment {
				break
			}
			at += n
		}
		return &stmt.NotSupportedError{What: "DROP INDEX", Offset: at}
	}
	return nil
}
