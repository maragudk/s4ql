package engine

import (
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/token"
)

// orderLimitRefusal of the statement s of the text src: for a select with ORDER BY or LIMIT, of slice
// 13, which the pass supports and the engine does not run yet, the "not supported" that the pass gave
// such a statement before it supported it, and nil for any other statement. The pass named the
// ORDER BY clause at its ORDER, or for a select without ORDER BY the LIMIT clause at its LIMIT. The
// statement has the offsets of the columns of ORDER BY and of the number of LIMIT, so the keyword is
// the last token of its kind in src before them. A text that the pass refuses for a token in ORDER
// BY or LIMIT, such as `order by 1`, gets the pass's error for that token, which it did not get
// before.
func orderLimitRefusal(src string, s stmt.Statement) error {
	sel, ok := s.(*stmt.Select)
	if !ok {
		return nil
	}
	what, kind, before := "ORDER BY clause", token.Order, 0
	switch {
	case sel.OrderBy != nil:
		before = sel.OrderBy[0].Name.Offset
	case sel.Limited():
		what, kind, before = "LIMIT clause", token.Limit, sel.Limit.Span.Start
	default:
		return nil
	}
	at := 0
	for _, t := range token.Tokens(src[:before]) {
		if t.Kind == kind {
			at = t.Offset
		}
	}
	return &stmt.NotSupportedError{What: what, Offset: at}
}
