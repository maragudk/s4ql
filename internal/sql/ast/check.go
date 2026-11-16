package ast

import (
	"cmp"
	"fmt"
	"slices"
)

// CheckOwnership of the tokens of the tree from root: that every token in read, the tokens that the
// parser read for the statement in order, is owned by exactly one node of the tree, that the tree
// owns no other token, and that the span of every node covers exactly its own tokens and its
// children. A node without tokens, such as an empty argument of a virtual table, must have an empty
// span inside the span of its parent. The own tokens and the children of a node must not overlap.
func CheckOwnership(root Node, read []Token) error {
	if err := checkSpans(root); err != nil {
		return err
	}
	owned := AllTokens(root)
	for i := 0; i < len(owned) || i < len(read); i++ {
		switch {
		case i >= len(owned):
			return fmt.Errorf("token %q at %v is owned by no node", read[i].Text, read[i].Offset)
		case i >= len(read):
			return fmt.Errorf("token %q at %v is owned by a node, but was not read", owned[i].Text, owned[i].Offset)
		case owned[i] != read[i]:
			return fmt.Errorf("token %d of the tree is %q at %v of kind %v, but the token read is %q at %v of kind %v",
				i, owned[i].Text, owned[i].Offset, owned[i].Kind, read[i].Text, read[i].Offset, read[i].Kind)
		}
	}
	return nil
}

// part of a node: one of its own tokens or one of its children, by the bytes it covers.
type part struct {
	span  Span
	empty bool
	what  string
}

// checkSpans of n and of every node under it.
func checkSpans(n Node) error {
	var parts []part
	for _, t := range Tokens(n) {
		if t.Text == "" {
			return fmt.Errorf("%T owns an empty token at %v", n, t.Offset)
		}
		parts = append(parts, part{span: Span{t.Offset, t.End()}, what: fmt.Sprintf("token %q", t.Text)})
	}
	for _, c := range Children(n) {
		if err := checkSpans(c); err != nil {
			return err
		}
		s := c.Bounds()
		parts = append(parts, part{span: s, empty: s.Start == s.End, what: fmt.Sprintf("%T", c)})
	}
	s := n.Bounds()
	if s.Start > s.End {
		return fmt.Errorf("%T has the span %v", n, s)
	}
	var covered []part
	for _, p := range parts {
		if !p.empty {
			covered = append(covered, p)
		}
	}
	if len(covered) == 0 {
		if s.Start != s.End {
			return fmt.Errorf("%T has no tokens, but its span is %v", n, s)
		}
	} else {
		slices.SortFunc(covered, func(a, b part) int {
			return cmp.Or(cmp.Compare(a.span.Start, b.span.Start), cmp.Compare(a.span.End, b.span.End))
		})
		if want := (Span{covered[0].span.Start, covered[len(covered)-1].span.End}); s != want {
			return fmt.Errorf("%T has the span %v, but its tokens and children cover %v", n, s, want)
		}
		for i := 1; i < len(covered); i++ {
			if covered[i].span.Start < covered[i-1].span.End {
				return fmt.Errorf("in %T, %v at %v overlaps %v at %v", n, covered[i].what, covered[i].span, covered[i-1].what, covered[i-1].span)
			}
		}
	}
	for _, p := range parts {
		if p.empty && (p.span.Start < s.Start || p.span.End > s.End) {
			return fmt.Errorf("in %T with the span %v, the empty %v is at %v", n, s, p.what, p.span)
		}
	}
	return nil
}
