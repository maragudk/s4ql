package parser

import (
	"fmt"

	"maragu.dev/s4ql/internal/sql/token"
)

// TraceToken is a token as the parser hands it on: its kind as the parser sees it, its offset, and
// its length. The empty semicolon at the end has length 0, and the end of input the kind
// [TraceEOF].
type TraceToken struct {
	Kind   token.Kind
	Offset int
	Len    int
}

// TraceEOF is the kind of the end of input in a [Trace].
const TraceEOF = kindEOF

// Trace of a parse of the first statement of src.
type Trace struct {
	// Tokens read up to the end of the statement, or up to and including the token of the error.
	Tokens []TraceToken
	// OK is true if the statement parses, or if the text has no statement.
	OK bool
	// ErrAt is the index in Tokens of the token of a syntax error.
	ErrAt int
	// MaxDepth of nesting reached.
	MaxDepth int
}

// TraceParse parses the first statement of src and gives the tokens that the parser read.
func TraceParse(src string) Trace {
	p := newParser(src, 0, textEnd(src))
	_, _, _ = p.run()
	tr := Trace{OK: p.err == nil, ErrAt: p.errAt, MaxDepth: p.maxDepth}
	n := p.i
	if p.err != nil {
		n = p.errAt + 1
	}
	for _, t := range p.toks[:min(n, len(p.toks))] {
		tr.Tokens = append(tr.Tokens, TraceToken{Kind: t.kind, Offset: t.off, Len: t.n})
	}
	return tr
}

// IsFallback is isFallback.
var IsFallback = isFallback

// BuiltinFunctions of the table of the parser, as "name nArg constant".
func BuiltinFunctions() []string {
	var fs []string
	for _, f := range builtinFunctions {
		fs = append(fs, fmt.Sprintf("%v %v %v", f.name, f.nArg, f.constant))
	}
	return fs
}
