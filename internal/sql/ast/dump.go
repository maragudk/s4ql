package ast

import (
	"reflect"
	"strings"
)

// Dump the tree from n as one line of text, for tests and debugging: each node as its type and its
// fields that are present, in the order of their declaration, such as
// `Binary{X: Literal{Value: 1}, Op: +, Y: Literal{Value: 2}}`. A token is its text, and a list is
// in brackets. Spans are left out.
func Dump(n Node) string {
	var b strings.Builder
	dumpNode(&b, n)
	return b.String()
}

func dumpNode(b *strings.Builder, n Node) {
	v := structOf(n)
	b.WriteString(v.Type().Name())
	b.WriteByte('{')
	first := true
	dumpFields(b, v, planOf(v.Type()), &first)
	b.WriteByte('}')
}

func dumpFields(b *strings.Builder, v reflect.Value, plan []field, first *bool) {
	sep := func(name string) {
		if !*first {
			b.WriteString(", ")
		}
		*first = false
		b.WriteString(name)
		b.WriteString(": ")
	}
	for _, f := range plan {
		fv := v.FieldByIndex(f.index)
		name := v.Type().FieldByIndex(f.index).Name
		switch f.kind {
		case fieldToken:
			if t := fv.Interface().(Token); t.Present() {
				sep(name)
				b.WriteString(t.Text)
			}
		case fieldTokens:
			if ts := fv.Interface().([]Token); len(ts) > 0 {
				sep(name)
				b.WriteByte('[')
				for i, t := range ts {
					if i > 0 {
						b.WriteByte(' ')
					}
					b.WriteString(t.Text)
				}
				b.WriteByte(']')
			}
		case fieldNode:
			if !fv.IsNil() {
				sep(name)
				dumpNode(b, fv.Interface().(Node))
			}
		case fieldNodes:
			if fv.Len() > 0 {
				sep(name)
				b.WriteByte('[')
				for i := range fv.Len() {
					if i > 0 {
						b.WriteString(", ")
					}
					if e := fv.Index(i); e.IsNil() {
						b.WriteString("nil")
					} else {
						dumpNode(b, e.Interface().(Node))
					}
				}
				b.WriteByte(']')
			}
		case fieldStruct:
			// The fields of a struct that is not a node are written as if they were the node's own,
			// prefixed by the struct field's name.
			inner := &strings.Builder{}
			innerFirst := true
			dumpFields(inner, fv, f.plan, &innerFirst)
			if inner.Len() > 0 {
				sep(name)
				b.WriteByte('{')
				b.WriteString(inner.String())
				b.WriteByte('}')
			}
		case fieldStructPtr:
			if !fv.IsNil() {
				inner := &strings.Builder{}
				innerFirst := true
				dumpFields(inner, fv.Elem(), f.plan, &innerFirst)
				sep(name)
				b.WriteByte('{')
				b.WriteString(inner.String())
				b.WriteByte('}')
			}
		}
	}
}
