package engine

import (
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/token"
)

// defaultRefusal of the statement s of the text src: for a create table with a column that says
// DEFAULT, of slice 11, which the pass supports and the engine does not run yet, the "not supported"
// that the pass gave such a statement before it supported DEFAULT, and nil for any other statement.
// The pass named the first DEFAULT of the statement at its keyword, the last token before the
// literal of the DEFAULT that is not a space or a comment. A text with a DEFAULT that the pass
// refuses for another token, such as an unknown type or a second statement, gets the pass's error
// for that token, which it did not get before when the token came after the DEFAULT.
func defaultRefusal(src string, s stmt.Statement) error {
	c, ok := s.(*stmt.CreateTable)
	if !ok {
		return nil
	}
	for _, column := range c.Columns {
		if column.Default.Value == nil {
			continue
		}
		keyword := -1
		for at := c.Span.Start; at < column.Default.Span.Start; {
			kind, n := token.Next(src[at:])
			if kind != token.Space && kind != token.Comment {
				keyword = at
			}
			at += n
		}
		return &stmt.NotSupportedError{What: "DEFAULT", Offset: keyword}
	}
	return nil
}
