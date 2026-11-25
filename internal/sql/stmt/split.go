package stmt

import (
	"strings"
	"unicode"

	"maragu.dev/s4ql/internal/sql/token"
)

// Part of a text of statements that are run one after another: one statement of the text, and the
// part of the text that it runs as.
type Part struct {
	// Statement of the part, with its spans in the whole text.
	Statement Statement
	// Span of the part in the whole text: from the end of the part before it, or the start of the
	// text, to the end of the semicolon of its statement, or, for the last part, to the end of the
	// text. So the empty statements, spaces, and comments before a statement are in its part, and
	// those after the last statement in the last part, and the parts cover the text.
	Span Span
}

// Split the SQL text src into its statements, for a run of them one after another, as the driver
// mattn/go-sqlite3 runs a text with several statements in its Exec: it prepares the first statement
// of the text with sqlite3_prepare_v2, runs it, and then prepares the first statement of the tail,
// the text after its semicolon, with the spaces at both ends of the tail removed by Go's
// strings.TrimSpace, until the tail is empty.
//
// It gives the parts of the statements that it read, in the order of the text, and the error that
// ended the reading, or nil. The error is one of these:
//
//  1. An error of the whole text, with no part: those that [Check] gives before it parses, and
//     "not supported" for a space that Go's TrimSpace removes and that SQLite reads as part of a
//     token, so that SQLite and mattn would read the text differently: at the start of a tail, as
//     "space after a statement that SQLite reads as part of a token", such as a no-break space or
//     a vertical tab before a statement, and, if a tail follows the first statement, at the end of
//     the text, as "space at the end of the text that SQLite reads as part of a token", such as a
//     no-break space after the last semicolon, or a space at the end of a string that has no
//     closing quote, which TrimSpace takes from SQLite's message. A space at the end of a comment
//     is no such space: the comment is a comment without it too.
//  2. The error of the first statement that [Check] would refuse, SQLite's syntax error or "not
//     supported", with the parts of the statements before it.
//
// A text without a statement gives no part and no error. Every offset is in src, of an error and
// of the spans of a statement. The first statement is read from src as Check reads it, and every
// later one from src without the spaces at its end, as mattn hands the tail to SQLite, which shows
// in the text that SQLite keeps of a CREATE TABLE or a CREATE INDEX without a semicolon.
func Split(src string) ([]Part, error) {
	if err := checkWhole(src); err != nil {
		return nil, err
	}
	var parts []Part
	view := src
	at := 0
	for {
		s, tail, err := statementAt(view, at)
		if err != nil {
			return parts, err
		}
		if s == nil {
			if len(parts) > 0 {
				parts[len(parts)-1].Span.End = len(src)
			}
			return parts, nil
		}
		if at == 0 && tail < len(src) {
			if err := trimmedEnd(src, tail); err != nil {
				return nil, err
			}
			// The tail ends after a semicolon, so the view holds it.
			view = strings.TrimRightFunc(src, unicode.IsSpace)
		}
		parts = append(parts, Part{Statement: s, Span: Span{Start: at, End: tail}})
		at = tail
		if err := trimmedStart(src, at); err != nil {
			return nil, err
		}
	}
}

// trimmedStart is the check of the start of the tail of src at the offset at, which Go's TrimSpace
// removes the spaces of: "not supported" at the first token among them that is not one of SQLite's
// spaces, or nil.
func trimmedStart(src string, at int) error {
	end := len(src) - len(strings.TrimLeftFunc(src[at:], unicode.IsSpace))
	// Each of SQLite's spaces is one of Go's, so a token of spaces that starts before end ends there
	// or before.
	for pos := at; pos < end; {
		kind, n := token.Next(src[pos:])
		if kind != token.Space {
			return &NotSupportedError{What: "space after a statement that SQLite reads as part of a token", Offset: pos}
		}
		pos += n
	}
	return nil
}

// trimmedEnd is the check of the end of src, which Go's TrimSpace removes the spaces of once a tail
// follows the first statement, whose tail is at the offset from: "not supported" if the last token
// of the text after from that is not one of SQLite's spaces holds some of those spaces, and is not a
// comment that stays one without them; otherwise nil.
func trimmedEnd(src string, from int) error {
	end := len(strings.TrimRightFunc(src, unicode.IsSpace))
	if end == len(src) {
		return nil
	}
	last, lastEnd, lastKind := -1, 0, token.Illegal
	for pos := from; pos < len(src); {
		kind, n := token.Next(src[pos:])
		if kind != token.Space {
			last, lastEnd, lastKind = pos, pos+n, kind
		}
		pos += n
	}
	if last < 0 || lastEnd <= end {
		return nil
	}
	if lastKind == token.Comment && last < end {
		// A line comment, and a block comment without its end, which runs to the end of the text.
		if kind, n := token.Next(src[last:end]); kind == token.Comment && n == end-last {
			return nil
		}
	}
	return &NotSupportedError{What: "space at the end of the text that SQLite reads as part of a token", Offset: max(last, end)}
}
