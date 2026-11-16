// Package parser reads SQL text into the syntax tree of internal/sql/ast, and accepts and rejects
// exactly what SQLite 3.54.1 does, with SQLite's syntax errors.
//
// The grammar is SQLite's own src/parse.y at 3.54.1, with the compile options of the binding in
// internal/sqlite, as the Lemon parser generator turns it into an LALR(1) parser: with its operator
// precedence, its %fallback of keywords to names, its %wildcard, and its token classes. This package
// is a hand-written recursive-descent parser for that grammar, with precedence climbing for
// expressions. Every non-trivial function names the rule or rules of parse.y that it reads. The
// parser keeps everything as written; a pass after it rejects what s4ql does not support, and raises
// the errors that SQLite's grammar actions raise, which are not syntax errors. The README next to this
// file lists those errors, the limits of the parser, and the places where SQLite's behaviour is not
// what one would guess.
//
// [Parse] reads the first statement of a text as sqlite3_prepare_v2 does: up to the first NUL byte,
// skipping empty statements, and it says where the statement ends. [ParseAll] reads every statement
// of a text in turn.
package parser

import (
	"fmt"
	"strings"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/token"
)

// Error of the parser: a syntax error in SQLite's words, or a limit of this parser.
type Error struct {
	// Message of the error. A syntax error has one of SQLite's three messages, `near "X": syntax
	// error`, `incomplete input`, or `unrecognized token: "X"`. A limit of this parser has a message
	// that starts with "s4ql: ", which none of SQLite's does.
	Message string

	// Offset of the error in bytes from the start of the text, or -1 where SQLite gives none, as for
	// `incomplete input`.
	Offset int

	// Limit is true if the error is a limit of this parser, such as the depth of nesting, and not a
	// syntax error. SQLite fails on such a text too, with an error of its own that is not a syntax
	// error.
	Limit bool
}

func (e *Error) Error() string {
	return e.Message
}

// MaxDepth of nesting that the parser reads. Each level is an expression or a SELECT inside another,
// and each holds at least one entry of the stack of SQLite's parser, whose depth SQLite limits to
// 2500 (SQLITE_LIMIT_PARSER_DEPTH), so a text that this limit rejects SQLite rejects too.
const MaxDepth = 2500

// MaxLength of a statement in bytes, SQLite's SQLITE_LIMIT_SQL_LENGTH: SQLite refuses a text whose
// tokens add up to more, counted from the start of the text, spaces and comments included.
const MaxLength = 1_000_000_000

// lengthLimit is [MaxLength], and smaller in tests.
var lengthLimit = MaxLength

// Parse the first statement of src. It reads src up to its first NUL byte, as SQLite does, skips
// empty statements, and returns the statement and the tail: the offset of the first byte after the
// statement's semicolon, or of the end of the text. A text with no statement gives a nil statement
// and no error, with the tail at the end of the text. On an error it returns an [*Error] and no
// statement.
func Parse(src string) (*ast.Statement, int, error) {
	return parseAt(src, 0, textEnd(src))
}

// ParseAll statements of src, each as [Parse] would from where the one before it ends, until the end
// of the text or the first error.
func ParseAll(src string) ([]*ast.Statement, error) {
	var stmts []*ast.Statement
	at, end := 0, textEnd(src)
	for {
		stmt, tail, err := parseAt(src, at, end)
		if err != nil {
			return stmts, err
		}
		if stmt == nil {
			return stmts, nil
		}
		stmts = append(stmts, stmt)
		at = tail
	}
}

// Tokens that [Parse] reads for the statement that starts at or after start in src: every token that
// is not a space or a comment, from the first token of the statement to its semicolon, if it has one.
// The empty statements before it are skipped. The tokens are as the tree holds them.
func Tokens(src string, stmt *ast.Statement) []ast.Token {
	var tokens []ast.Token
	s := stmt.Bounds()
	for _, t := range token.Tokens(src) {
		if t.Offset < s.Start || t.Offset >= s.End || t.Kind == token.Space || t.Kind == token.Comment {
			continue
		}
		tokens = append(tokens, ast.Token{Kind: t.Kind, Offset: t.Offset, Text: t.Text(src)})
	}
	return tokens
}

// kindEOF is the kind of the token after the end of the text: the end of input, $ in Lemon, which
// the parse loop of SQLite hands to the parser after the empty semicolon.
const kindEOF token.Kind = 255

// tok is a token as the parser reads it.
type tok struct {
	// kind as the parser sees it: the kind from the tokenizer, except that WINDOW, OVER, and FILTER
	// become ID where the parse loop decides so, and kindEOF for the end of input.
	kind token.Kind
	// raw kind from the tokenizer.
	raw token.Kind
	off int
	n   int
}

// synthetic reports whether the token is the empty semicolon that SQLite's parse loop hands to the
// parser at the end of the text.
func (t tok) synthetic() bool {
	return t.kind == token.Semi && t.n == 0
}

// end of input: the empty semicolon, or the end itself.
func (t tok) atEnd() bool {
	return t.kind == kindEOF || t.synthetic()
}

// bailout is the panic with which a syntax error unwinds the parser, recovered in parseAt.
type bailout struct{}

// pendingError is an error that SQLite raises in a grammar action when the token after the one it
// checks arrives: a variable #N, or a misplaced digit separator. The error is SQLite's unless that
// next token is a syntax error.
type pendingError struct {
	message string
	offset  int
	// trigger is the index of the token on whose arrival SQLite raises the error.
	trigger int
	// variable is true for a #N, whose offset can be lost; see [parser.result].
	variable bool
}

// parser of one statement.
type parser struct {
	src string
	// end of the text: the offset of its first NUL byte, or its length.
	end int
	// toks read so far, without spaces and comments.
	toks []tok
	// start of the text to parse, next offset in src for the tokenizer, and whether the end, or an
	// unrecognized token, has been reached.
	start   int
	next    int
	atEnd   bool
	illegal bool
	// i is the index in toks of the current token.
	i int
	// lastEnd is the end offset of the last token consumed.
	lastEnd int
	depth   int
	// maxDepth reached, for the tests.
	maxDepth int
	err      *Error
	errAt    int
	pending  *pendingError
}

// textEnd of src: the offset of its first NUL byte, or its length.
func textEnd(src string) int {
	if i := strings.IndexByte(src, 0); i >= 0 {
		return i
	}
	return len(src)
}

// newParser of the first statement of src that starts at or after the offset start, where the text
// ends at end.
func newParser(src string, start, end int) *parser {
	return &parser{src: src, end: end, start: start, next: start, lastEnd: start}
}

// parseAt parses the first statement of src that starts at or after the offset start, where the
// text ends at end.
func parseAt(src string, start, end int) (*ast.Statement, int, error) {
	return newParser(src, start, end).run()
}

// run the parser.
func (p *parser) run() (stmt *ast.Statement, tail int, err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(bailout); !ok {
				panic(r)
			}
			stmt, tail, err = nil, 0, p.result(nil)
		}
	}()
	stmt = p.statement()
	if stmt == nil {
		return nil, p.end, nil
	}
	if err := p.result(stmt); err != nil {
		return nil, 0, err
	}
	if stmt.Semi.Present() {
		return stmt, stmt.Semi.End(), nil
	}
	return stmt, p.end, nil
}

// result of the parse: the syntax error p.err at token p.errAt, if any, or the pending error of a
// variable #N or a digit separator, which SQLite raises when the token after it arrives and which is
// lost only if that token is the syntax error.
func (p *parser) result(stmt *ast.Statement) error {
	pe := p.pending
	if pe == nil || (p.err != nil && p.errAt <= pe.trigger) {
		if p.err != nil {
			return p.err
		}
		return nil
	}
	offset := pe.offset
	if pe.variable && stmt != nil && p.endsAt(stmt, pe.trigger) && resetsOffset(stmt.Cmd) {
		// SQLite raises the error of #N when the semicolon at the end arrives, and the action of the
		// command, which runs on that semicolon too, reads the schema of the new connection first.
		// That runs SQL of its own, which leaves the offset of the error at -1.
		offset = -1
	}
	return &Error{Message: pe.message, Offset: offset}
}

// endsAt reports whether the token at index i is the semicolon that ends stmt, or the empty
// semicolon at the end of the text after it.
func (p *parser) endsAt(stmt *ast.Statement, i int) bool {
	t := p.toks[i]
	return t.kind == token.Semi && t.off >= stmt.Cmd.Bounds().End
}

// resetsOffset reports whether the action of the command reads the schema even after an error in
// the statement, as `cmd ::= select` does with sqlite3ReadSchema, sqlite3CreateView through
// sqlite3StartTable, and sqlite3Attach and sqlite3Detach through codeAttach.
func resetsOffset(c ast.Cmd) bool {
	switch c.(type) {
	case *ast.SelectCmd, *ast.CreateView, *ast.Attach, *ast.Detach:
		return true
	}
	return false
}

// tokAt index i, reading more of the text if needed.
func (p *parser) tokAt(i int) tok {
	for len(p.toks) <= i {
		p.read()
	}
	return p.toks[i]
}

// read the next token that is not a space or a comment into p.toks: as the parse loop
// sqlite3RunParser in tokenize.c hands tokens to the parser.
func (p *parser) read() {
	if p.atEnd || p.illegal {
		// Past the end, or past an unrecognized token, the parser never reads; a lookahead there sees
		// the same token again.
		p.toks = append(p.toks, p.toks[len(p.toks)-1])
		return
	}
	for {
		kind, n := token.Next(p.src[p.next:p.end])
		if n == 0 {
			// At the end of the input, SQLite hands the parser an empty semicolon, unless the last
			// token was a semicolon, and then the end itself.
			p.atEnd = true
			if len(p.toks) == 0 || p.toks[len(p.toks)-1].kind != token.Semi {
				p.toks = append(p.toks, tok{kind: token.Semi, raw: token.Semi, off: p.next})
			}
			p.toks = append(p.toks, tok{kind: kindEOF, raw: kindEOF, off: p.next})
			return
		}
		t := tok{kind: kind, raw: kind, off: p.next, n: n}
		p.next += n
		if p.next-p.start > lengthLimit {
			p.overLimit("s4ql: the statement is longer than the limit of 1000000000 bytes")
		}
		switch kind {
		case token.Space, token.Comment:
			continue
		case token.Illegal:
			p.illegal = true
		case token.Window:
			t.kind = analyzeWindowKeyword(p.src[p.next:p.end])
		case token.Over:
			t.kind = analyzeOverKeyword(p.src[p.next:p.end], p.lastKind())
		case token.Filter:
			t.kind = analyzeFilterKeyword(p.src[p.next:p.end], p.lastKind())
		}
		p.toks = append(p.toks, t)
		return
	}
}

// lastKind of the token read before the one being read, as the parser saw it, or 0 if none.
func (p *parser) lastKind() token.Kind {
	if len(p.toks) == 0 {
		return 0
	}
	return p.toks[len(p.toks)-1].kind
}

// getToken is getToken in tokenize.c: the kind of the next token of z that is not a space or a
// comment, as ID for any token that can be a name there, and the rest of z after it.
func getToken(z string) (token.Kind, string) {
	for {
		kind, n := token.Next(z)
		z = z[n:]
		if kind == token.Space || kind == token.Comment {
			continue
		}
		if kind == token.ID || kind == token.String || kind == token.JoinKw || kind == token.Window ||
			kind == token.Over || isFallback(kind) {
			kind = token.ID
		}
		return kind, z
	}
}

// analyzeWindowKeyword is analyzeWindowKeyword in tokenize.c: WINDOW is a keyword if a name and AS
// follow it.
func analyzeWindowKeyword(z string) token.Kind {
	t, z := getToken(z)
	if t != token.ID {
		return token.ID
	}
	if t, _ = getToken(z); t != token.As {
		return token.ID
	}
	return token.Window
}

// analyzeOverKeyword is analyzeOverKeyword in tokenize.c: OVER is a keyword after ")" and before
// "(" or a name.
func analyzeOverKeyword(z string, last token.Kind) token.Kind {
	if last == token.RP {
		if t, _ := getToken(z); t == token.LP || t == token.ID {
			return token.Over
		}
	}
	return token.ID
}

// analyzeFilterKeyword is analyzeFilterKeyword in tokenize.c: FILTER is a keyword after ")" and
// before "(".
func analyzeFilterKeyword(z string, last token.Kind) token.Kind {
	if last == token.RP {
		if t, _ := getToken(z); t == token.LP {
			return token.Filter
		}
	}
	return token.ID
}

// cur is the current token.
func (p *parser) cur() tok {
	return p.tokAt(p.i)
}

// peek at the token k places after the current one.
func (p *parser) peek(k int) tok {
	return p.tokAt(p.i + k)
}

// is reports whether the current token is of the kind k as the parser sees it.
func (p *parser) is(k token.Kind) bool {
	return p.cur().kind == k
}

// advance past the current token and return it as a token of the tree.
func (p *parser) advance() ast.Token {
	t := p.cur()
	if t.atEnd() || t.kind == token.Illegal {
		panic("parser: advance past the end or an unrecognized token")
	}
	p.i++
	p.lastEnd = t.off + t.n
	return ast.Token{Kind: t.raw, Offset: t.off, Text: p.src[t.off : t.off+t.n]}
}

// accept the current token if it is of the kind k.
func (p *parser) accept(k token.Kind) ast.Token {
	if p.is(k) {
		return p.advance()
	}
	return ast.Token{}
}

// expect a token of the kind k, or fail with a syntax error at the current token.
func (p *parser) expect(k token.Kind) ast.Token {
	if !p.is(k) {
		p.fail()
	}
	return p.advance()
}

// fail with a syntax error at the current token, as Lemon's parser does on a token for which it has
// no action: `%syntax_error` in parse.y gives `near "X": syntax error`, or `incomplete input` for the
// empty token at the end, and the parse loop gives `unrecognized token: "X"` for a token that the
// tokenizer does not know, before the parser sees it.
func (p *parser) fail() {
	t := p.cur()
	switch {
	case t.atEnd():
		p.err = &Error{Message: "incomplete input", Offset: -1}
	case t.kind == token.Illegal:
		p.err = &Error{Message: fmt.Sprintf(`unrecognized token: "%s"`, p.src[t.off:t.off+t.n]), Offset: t.off}
	default:
		p.err = &Error{Message: fmt.Sprintf(`near "%s": syntax error`, p.src[t.off:t.off+t.n]), Offset: t.off}
	}
	p.errAt = p.i
	panic(bailout{})
}

// overLimit fails with an error of a limit of this parser, at the current token.
func (p *parser) overLimit(message string) {
	p.err = &Error{Message: message, Offset: -1, Limit: true}
	p.errAt = len(p.toks)
	panic(bailout{})
}

// enter one level of nesting, and fail if it is deeper than [MaxDepth].
func (p *parser) enter() {
	p.depth++
	p.maxDepth = max(p.maxDepth, p.depth)
	if p.depth > MaxDepth {
		p.overLimit(fmt.Sprintf("s4ql: the statement is nested deeper than the limit of %v", MaxDepth))
	}
}

// leave one level of nesting.
func (p *parser) leave() {
	p.depth--
}

// mark the start of a node at the current token.
func (p *parser) mark() int {
	return p.cur().off
}

// finish the node n that started at start, with the token consumed last as its end, and return it.
func finish[N ast.Node](p *parser, n N, start int) N {
	ast.SetSpan(n, ast.Span{Start: start, End: p.lastEnd})
	return n
}
