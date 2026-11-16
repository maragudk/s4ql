// Package ast is the syntax tree of SQLite's SQL as s4ql's parser in internal/sql/parser reads it.
//
// The tree keeps everything as written. A name keeps its raw text with its quotes, a literal its
// raw text, a keyword its spelling and case, and nothing is folded, normalized, or rewritten: the
// tree has the shape of the text, not the shape of SQLite's internal form. `x NOTNULL`, `x NOT NULL`,
// and `x IS NOT NULL` are three different trees, and `LIMIT a, b` keeps its operands in the order
// written. Every optional keyword that changes the meaning of a statement by being there, such as
// DISTINCT, ASC, TEMP, IF NOT EXISTS, or AS before an alias, is a [Token] of its own that is
// absent when it was not written.
//
// Every node records its [Span] in the source, and every token that it consumed directly, as a
// field of type [Token] or []Token. Every token that the parser read for a statement, from its first
// token to its last, the semicolon that ends it included, is owned by exactly one node, and the span
// of a node covers its own tokens and its children and nothing else. Spaces and comments are not
// tokens of the tree. [Children] and [Tokens] give the children and own tokens of any node from its
// fields, through reflection, so a check of these rules needs no list of node kinds that could go
// stale.
package ast

import (
	"fmt"
	"reflect"
	"slices"
	"sync"

	"maragu.dev/s4ql/internal/sql/token"
)

// Span of a node: the bytes [Start, End) of the source that its tokens and its children cover.
type Span struct {
	Start, End int
}

// Token read from the source: its kind as the tokenizer in internal/sql/token gives it, its offset
// in bytes from the start of the source, and its text as written. A keyword that the parser read as a
// name keeps the kind of the keyword. The zero Token is an absent token.
type Token struct {
	Kind   token.Kind
	Offset int
	Text   string
}

// Present reports whether the token was written: a token of the zero value is absent.
func (t Token) Present() bool {
	return t.Kind != token.Illegal
}

// End offset of the token: the offset of the first byte after it.
func (t Token) End() int {
	return t.Offset + len(t.Text)
}

func (t Token) String() string {
	if !t.Present() {
		return "<absent>"
	}
	return t.Text
}

// Node of the tree. Every node is a pointer to a struct that embeds base.
type Node interface {
	// Bounds of the node in the source.
	Bounds() Span
	node()
}

// base of every node, which holds its span.
type base struct {
	// Span of the node in the source.
	Span Span
}

// Bounds of the node in the source.
func (b *base) Bounds() Span { return b.Span }

func (*base) node() {}

// SetSpan of the node. It is for the parser, which knows the span only when the node is complete.
func (b *base) SetSpan(s Span) { b.Span = s }

// spanSetter is a node whose span can be set.
type spanSetter interface {
	SetSpan(Span)
}

// SetSpan of n to s.
func SetSpan(n Node, s Span) {
	n.(spanSetter).SetSpan(s)
}

var (
	nodeType  = reflect.TypeFor[Node]()
	tokenType = reflect.TypeFor[Token]()
	baseType  = reflect.TypeFor[base]()
)

// fieldKind is what a field of a node holds, for the walk.
type fieldKind int

const (
	fieldToken  fieldKind = iota // a Token
	fieldTokens                  // a []Token
	fieldNode                    // a Node: an interface or a pointer to a node
	fieldNodes                   // a slice of Nodes
	fieldStruct                  // a struct that is not a node, whose fields belong to the node
)

// field of a node type that the walk visits.
type field struct {
	index []int
	kind  fieldKind
	plan  []field // for fieldStruct: the fields of the struct
}

// plans of the node types, by type of the struct, built on first use.
var plans sync.Map

// planOf the struct type t: the fields that hold tokens or nodes, in declaration order. It panics on
// a field of any other type, so that a token or node cannot hide in a field that the walk does not
// see. Fields of kinds that hold no token, such as bool or int, are not allowed either: a node keeps
// what was written as tokens.
func planOf(t reflect.Type) []field {
	if p, ok := plans.Load(t); ok {
		return p.([]field)
	}
	var fs []field
	for i := range t.NumField() {
		sf := t.Field(i)
		if sf.Type == baseType {
			continue
		}
		f := field{index: sf.Index}
		ft := sf.Type
		switch {
		case ft == tokenType:
			f.kind = fieldToken
		case ft.Kind() == reflect.Slice && ft.Elem() == tokenType:
			f.kind = fieldTokens
		case ft.Implements(nodeType):
			f.kind = fieldNode
		case ft.Kind() == reflect.Slice && ft.Elem().Implements(nodeType):
			f.kind = fieldNodes
		case ft.Kind() == reflect.Struct:
			f.kind = fieldStruct
			f.plan = planOf(ft)
		default:
			panic(fmt.Sprintf("ast: field %v.%v of type %v holds neither tokens nor nodes", t.Name(), sf.Name, ft))
		}
		fs = append(fs, f)
	}
	plans.Store(t, fs)
	return fs
}

// visit the tokens and children held in the struct v, in the order of its fields.
func visit(v reflect.Value, plan []field, tok func(Token), child func(Node)) {
	for _, f := range plan {
		fv := v.FieldByIndex(f.index)
		switch f.kind {
		case fieldToken:
			if t := fv.Interface().(Token); t.Present() {
				tok(t)
			}
		case fieldTokens:
			for _, t := range fv.Interface().([]Token) {
				tok(t)
			}
		case fieldNode:
			if !fv.IsNil() {
				child(fv.Interface().(Node))
			}
		case fieldNodes:
			for i := range fv.Len() {
				if e := fv.Index(i); !e.IsNil() {
					child(e.Interface().(Node))
				}
			}
		case fieldStruct:
			visit(fv, f.plan, tok, child)
		}
	}
}

// structOf the node n, which must not be nil. Every node is a pointer to a struct, since the methods
// of Node come from embedding base, whose methods have pointer receivers.
func structOf(n Node) reflect.Value {
	return reflect.ValueOf(n).Elem()
}

// Children of n that are present, in the order of its fields.
func Children(n Node) []Node {
	var children []Node
	v := structOf(n)
	visit(v, planOf(v.Type()), func(Token) {}, func(c Node) { children = append(children, c) })
	return children
}

// Tokens that n owns directly, not those of its children, sorted by offset.
func Tokens(n Node) []Token {
	var tokens []Token
	v := structOf(n)
	visit(v, planOf(v.Type()), func(t Token) { tokens = append(tokens, t) }, func(Node) {})
	slices.SortFunc(tokens, func(a, b Token) int { return a.Offset - b.Offset })
	return tokens
}

// Walk the tree from n in depth-first order: f is called for n and then, if it returns true, for
// each of its children in turn.
func Walk(n Node, f func(Node) bool) {
	if n == nil || !f(n) {
		return
	}
	for _, c := range Children(n) {
		Walk(c, f)
	}
}

// AllTokens of the tree from n, sorted by offset.
func AllTokens(n Node) []Token {
	var tokens []Token
	Walk(n, func(m Node) bool {
		tokens = append(tokens, Tokens(m)...)
		return true
	})
	slices.SortFunc(tokens, func(a, b Token) int { return a.Offset - b.Offset })
	return tokens
}
