package stmt

import (
	"errors"
	"unicode/utf8"

	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/token"
)

// Check the SQL text src, and give the statement that it holds if s4ql supports it.
//
// The answer is one of four, checked in this order:
//
//  1. A [*NotSupportedError] for text that is not valid UTF-8 or that has a NUL byte, at the first
//     byte that is either.
//  2. A [*SyntaxError] if the text is a syntax error in SQLite, with SQLite's result codes and
//     message.
//  3. No statement and no error if the text has no statement: if it is empty, or holds only spaces,
//     comments, and semicolons.
//  4. A [*NotSupportedError] if the first statement is not one that s4ql supports, or if another
//     statement follows it; otherwise the [Statement].
//
// Only the first statement is checked for syntax. A text with a second statement is not supported
// even if the second one is a syntax error, since SQLite prepares the first statement without
// reading the second.
func Check(src string) (Statement, error) {
	if at, what := checkText(src); at >= 0 {
		return nil, &NotSupportedError{What: what, Offset: at}
	}

	tree, tail, err := parser.Parse(src)
	if err != nil {
		var e *parser.Error
		if !errors.As(err, &e) {
			return nil, err
		}
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

// checkText for what s4ql rejects before it parses: it gives the offset of the first NUL byte or
// byte that is not valid UTF-8, and what it is, or -1 if src has neither.
func checkText(src string) (int, string) {
	for i := 0; i < len(src); {
		if src[i] == 0 {
			return i, "NUL byte"
		}
		if src[i] < utf8.RuneSelf {
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(src[i:])
		if r == utf8.RuneError && n == 1 {
			return i, "invalid UTF-8"
		}
		i += n
	}
	return -1, ""
}

// firstToken of src from at that is not a space, a comment, or a semicolon: its offset, or the length
// of src if there is none.
func firstToken(src string, at int) int {
	for at < len(src) {
		kind, n := token.Next(src[at:])
		if kind != token.Space && kind != token.Comment && kind != token.Semi {
			return at
		}
		at += n
	}
	return at
}
