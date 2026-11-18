package stmt

import (
	"unicode/utf8"

	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/token"
)

// lengthLimit is [parser.MaxLength], and smaller in tests.
var lengthLimit = parser.MaxLength

// Check the SQL text src, and give the statement that it holds if s4ql supports it.
//
// The answer is one of four, checked in this order:
//
//  1. A [*NotSupportedError] for text that is longer than [parser.MaxLength], which SQLite refuses
//     before it parses, and for text that is not valid UTF-8 or that has a NUL byte, at the first
//     byte that is either.
//  2. A [*SyntaxError] if the text is a syntax error in SQLite, with SQLite's result codes and
//     message. A text that the parser refuses for one of its limits, which SQLite refuses too with
//     an error that is not a syntax error, is not supported.
//  3. No statement and no error if the text has no statement: if it is empty, or holds only spaces,
//     comments, and semicolons.
//  4. A [*NotSupportedError] if the first statement is not one that s4ql supports, or if another
//     statement follows it; otherwise the [Statement].
//
// Only the first statement is checked for syntax. A text with a second statement is not supported
// even if the second one is a syntax error, since SQLite prepares the first statement without
// reading the second.
func Check(src string) (Statement, error) {
	if len(src) > lengthLimit {
		return nil, &NotSupportedError{What: "text longer than SQLite's limit", Offset: lengthLimit}
	}
	if err := checkText(src); err != nil {
		return nil, err
	}

	tree, tail, err := parser.Parse(src)
	if err != nil {
		// The parser gives only errors of its own type.
		e := err.(*parser.Error)
		if e.Limit {
			return nil, &NotSupportedError{What: "statement beyond a limit of the parser", Offset: firstToken(src, 0)}
		}
		return nil, &SyntaxError{Code: 1, ExtendedCode: 1, Message: e.Message, Offset: e.Offset}
	}
	if tree == nil {
		return nil, nil
	}

	s, ns := recognize(tree)
	if ns != nil {
		return nil, ns
	}
	if at := firstToken(src, tail); at < len(src) {
		return nil, &NotSupportedError{What: "second statement", Offset: at}
	}
	return s, nil
}

// checkText for what s4ql rejects before it parses: the first NUL byte or byte that is not valid
// UTF-8, or nil if src has neither.
func checkText(src string) *NotSupportedError {
	for i := 0; i < len(src); {
		r, n := utf8.DecodeRuneInString(src[i:])
		switch {
		case r == 0:
			return &NotSupportedError{What: "NUL byte", Offset: i}
		case r == utf8.RuneError && n == 1:
			return &NotSupportedError{What: "invalid UTF-8", Offset: i}
		}
		i += n
	}
	return nil
}

// firstToken of src from at that is not a space, a comment, or a semicolon: its offset, or the length
// of src if there is none. At the end of src, the tokenizer gives an Illegal token of length 0.
func firstToken(src string, at int) int {
	for {
		kind, n := token.Next(src[at:])
		if kind != token.Space && kind != token.Comment && kind != token.Semi {
			return at
		}
		at += n
	}
}
