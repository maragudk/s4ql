package engine

import (
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/token"
)

// existsRefusal of the statement s of the text src: for a create table or a create index that says
// IF NOT EXISTS, and a drop table or a drop index that says IF EXISTS, of slice 10, which the pass
// supports and the engine does not run yet, the "not supported" that the pass gave such a statement
// before it supported the clauses, and nil for any other statement. The pass named the clause at its
// IF, which is the third token of each of the four statements. A text with a clause that the pass
// refuses for a later token, such as an unknown type or a second statement, gets the pass's error
// for that token, which it did not get before.
func existsRefusal(src string, s stmt.Statement) error {
	what := ""
	switch s := s.(type) {
	case *stmt.CreateTable:
		if s.IfNotExists {
			what = "IF NOT EXISTS"
		}
	case *stmt.CreateIndex:
		if s.IfNotExists {
			what = "IF NOT EXISTS"
		}
	case *stmt.DropTable:
		if s.IfExists {
			what = "IF EXISTS"
		}
	case *stmt.DropIndex:
		if s.IfExists {
			what = "IF EXISTS"
		}
	}
	if what == "" {
		return nil
	}
	// The first token of the statement is at the start of its span; the IF is the second token after
	// it that is not a space or a comment.
	at := s.Bounds().Start
	for range 2 {
		_, n := token.Next(src[at:])
		at += n
		for {
			kind, n := token.Next(src[at:])
			if kind != token.Space && kind != token.Comment {
				break
			}
			at += n
		}
	}
	return &stmt.NotSupportedError{What: what, Offset: at}
}
