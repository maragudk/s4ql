package stmt

import (
	"unicode/utf8"

	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/sqlerr"
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
//  2. A [*sqlerr.Error] if the text is a syntax error in SQLite, with SQLite's result codes and
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
	if err := checkWhole(src); err != nil {
		return nil, err
	}
	s, tail, err := statementAt(src, 0)
	if err != nil || s == nil {
		return nil, err
	}
	if at := firstToken(src, tail); at < len(src) {
		return nil, &NotSupportedError{What: "second statement", Offset: at}
	}
	return s, nil
}

// checkWhole is the check of the text src before it is parsed: [NotSupportedError] for a text longer
// than SQLite's limit, and then as [checkText] says; otherwise nil.
func checkWhole(src string) error {
	if len(src) > lengthLimit {
		return &NotSupportedError{What: "text longer than SQLite's limit", Offset: lengthLimit}
	}
	if err := checkText(src); err != nil {
		return err
	}
	return nil
}

// statementAt of src: the first statement that starts at or after the offset at, as [parser.ParseAt]
// reads it, and its tail, if s4ql supports it. Its errors are those of [Check], after the checks of
// the whole text, with their offsets in src: SQLite's syntax error, and "not supported" for a limit
// of the parser and for a statement that s4ql does not support. A rest without a statement gives no
// statement and no error, with the tail at the end of the text.
func statementAt(src string, at int) (Statement, int, error) {
	tree, tail, err := parser.ParseAt(src, at)
	if err != nil {
		// The parser gives only errors of its own type.
		e := err.(*parser.Error)
		if e.Limit {
			return nil, 0, &NotSupportedError{What: "statement beyond a limit of the parser", Offset: firstToken(src, at)}
		}
		return nil, 0, &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: e.Message, Offset: e.Offset}
	}
	if tree == nil {
		return nil, tail, nil
	}
	s, ns := recognize(src, tree)
	if ns != nil {
		return nil, 0, ns
	}
	return s, tail, nil
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
