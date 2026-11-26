package expr_test

import (
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"strings"

	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

// generator of cases at random. Two cases in three are trees of arithmetic, +, -, *, /, and
// negations, of varying depth and shape,
// whose leaves are literals, columns, and parameters with integers, reals, and NULL that weigh the
// edges, and often with values that are related so that an operation reaches its edge: a sum, a
// difference, a product, or a quotient just inside the integers of 64 bits and just outside, a
// divisor that is zero or an operation that gives zero under a division, reals whose operation is
// infinite or underflows, a product whose sum or difference is zero or next to it where a fused
// instruction would give another result, and integers above 2^53 with reals. One case in 25 has a
// text or a blob as an operand, from a column or an argument, and one in 40 is a column alone. The
// third case is a tree of text, as [generator.textCase] says.
type generator struct {
	r *rand.Rand
	b strings.Builder
	// row of the case, with nil for a column that no leaf has taken yet.
	row  []*s4qlpb.Value
	args []*s4qlpb.Value
	// limit of the case, or 0 for [maxLength].
	limit int
	// deep reports whether the case is a tree of [generator.deepTree], which has no parentheses that it
	// does not need, since its own come near the limit of the pass.
	deep bool
	// nullIn is the place of the leaf that the generator writes, which decides the columns that may
	// hold NULL for it.
	nullIn nullIn
}

// nullIn is the place of a leaf, for NULL: in arithmetic, where a column of a number holds it, in
// text, where a column of text does, or as the argument of hex or a column alone, where a column of
// text or of a blob does.
type nullIn uint8

const (
	nullInNumber nullIn = iota
	nullInText
	nullInTextOrBlob
)

// newGenerator of the seed.
func newGenerator(seed uint64) *generator {
	return &generator{r: rand.New(rand.NewPCG(seed, 0x5e4a))}
}

// gnode of a tree of the [generator]: an operator, or a leaf, of no kind, whose text, a literal, a
// column, or a parameter, is chosen when it is written, after its value.
type gnode struct {
	kind     stmt.ExpressionKind
	function stmt.Function
	operands []*gnode
	value    *s4qlpb.Value
}

// leaf reports whether n is a leaf.
func (n *gnode) leaf() bool {
	return n.kind == 0
}

// binary reports whether n is an operator of two leaves.
func (n *gnode) binary() bool {
	return len(n.operands) == 2 && n.operands[0].leaf() && n.operands[1].leaf()
}

// next case of the generator.
func (g *generator) next() exprCase {
	g.b.Reset()
	g.row = make([]*s4qlpb.Value, len(schema))
	g.args = nil
	g.limit, g.nullIn, g.deep = 0, nullInNumber, false
	if g.r.IntN(3) == 0 {
		return g.textCase()
	}
	var root *gnode
	switch depth := 1 + g.r.IntN(5); {
	case g.r.IntN(40) == 0:
		root = &gnode{}
	case g.r.IntN(20) == 0:
		root = g.operator(depth + 6)
	default:
		root = g.operator(depth)
	}
	g.relate(root)
	g.fill(root)
	if g.r.IntN(25) == 0 {
		g.plant(root)
	}
	if root.leaf() {
		g.column(root.value)
	} else {
		g.write(root, 0, false)
	}
	return g.done()
}

// done is the case that the generator wrote, with a value for each column that no leaf took.
func (g *generator) done() exprCase {
	for i, v := range g.row {
		if v == nil {
			g.row[i] = g.columnValue(i)
		}
	}
	return exprCase{src: g.b.String(), row: g.row, args: g.args, limit: g.limit}
}

// shape of a node of at most depth levels of operators: a leaf three times in ten, and otherwise an
// operator.
func (g *generator) shape(depth int) *gnode {
	if depth == 0 || g.r.IntN(10) < 3 {
		return &gnode{}
	}
	return g.operator(depth)
}

// operator of at most depth levels: a negation one time in six, a sum or a difference of a product
// of two leaves and a leaf one time in six, and a binary operator otherwise.
func (g *generator) operator(depth int) *gnode {
	switch g.r.IntN(6) {
	case 0:
		return &gnode{kind: stmt.ExprNegate, operands: []*gnode{g.shape(depth - 1)}}
	case 1:
		m := &gnode{kind: stmt.ExprMultiply, operands: []*gnode{{}, {}}}
		operands := []*gnode{m, {}}
		if g.r.IntN(2) == 0 {
			operands[0], operands[1] = operands[1], operands[0]
		}
		return &gnode{kind: []stmt.ExpressionKind{stmt.ExprAdd, stmt.ExprSubtract}[g.r.IntN(2)], operands: operands}
	}
	kind := []stmt.ExpressionKind{stmt.ExprAdd, stmt.ExprSubtract, stmt.ExprMultiply, stmt.ExprDivide}[g.r.IntN(4)]
	return &gnode{kind: kind, operands: []*gnode{g.shape(depth - 1), g.shape(depth - 1)}}
}

// relate the values of the leaves under n, from the root down, so that operations reach their edges.
func (g *generator) relate(n *gnode) {
	switch {
	case n.kind == stmt.ExprNegate && n.operands[0].leaf() && g.r.IntN(3) == 0:
		n.operands[0].value = []*s4qlpb.Value{integer(math.MinInt64), integer(math.MinInt64 + 1), float(0), float(math.Copysign(0, -1))}[g.r.IntN(4)]
	case g.fusable(n) && g.r.IntN(3) > 0:
		g.fuse(n)
	case n.binary() && g.r.IntN(2) == 0:
		g.edge(n)
	case n.kind == stmt.ExprDivide && n.operands[1].binary() && g.r.IntN(3) == 0:
		g.zero(n.operands[1])
	}
	for _, o := range n.operands {
		g.relate(o)
	}
}

// edge of the binary operator n of two leaves: two times in three values at the edges of the
// integers of 64 bits, and otherwise at those of the reals, or an integer above 2^53 with a real.
func (g *generator) edge(n *gnode) {
	l, r := n.operands[0], n.operands[1]
	switch g.r.IntN(6) {
	case 0, 1, 2, 3:
		x := g.integer()
		bound := []*big.Int{big.NewInt(math.MaxInt64), big.NewInt(math.MinInt64)}[g.r.IntN(2)]
		delta := big.NewInt(int64(g.r.IntN(3) - 1))
		y := new(big.Int)
		switch n.kind {
		case stmt.ExprAdd:
			// x + y is bound + delta.
			y.Sub(bound, big.NewInt(x)).Add(y, delta)
		case stmt.ExprSubtract:
			// x - y is bound - delta.
			y.Sub(big.NewInt(x), bound).Add(y, delta)
		case stmt.ExprMultiply:
			for x == 0 {
				x = g.integer()
			}
			if g.r.IntN(2) == 0 {
				x = int64(1+g.r.IntN(1<<20)) * int64(1-2*g.r.IntN(2))
			}
			y.Quo(bound, big.NewInt(x)).Add(y, delta)
		case stmt.ExprDivide:
			if g.r.IntN(2) == 0 {
				x = math.MinInt64
			}
			y.SetInt64(int64(g.r.IntN(3) - 1))
			if g.r.IntN(4) == 0 {
				// A negative divisor other than -1, which fits also for the smallest integer.
				y.SetInt64(-int64(2 + g.r.IntN(1000)))
			}
		}
		if !y.IsInt64() {
			y.SetInt64(g.integer())
		}
		l.value, r.value = integer(x), integer(y.Int64())
	case 4:
		sign := float64(1 - 2*g.r.IntN(2))
		x := sign * math.MaxFloat64 * (0.5 + g.r.Float64()/2)
		y := x
		switch n.kind {
		case stmt.ExprAdd:
			if g.r.IntN(3) == 0 {
				y = -x
			}
		case stmt.ExprSubtract:
			y = -x
			if g.r.IntN(3) == 0 {
				y = x
			}
		case stmt.ExprMultiply:
			e := []float64{1e154, 1e160, 1e200, 1e-160, 1e-200}[g.r.IntN(5)]
			x, y = sign*e*(1+g.r.Float64()), e*(1+g.r.Float64())
		case stmt.ExprDivide:
			x, y = sign*1e300, []float64{1e-10, 0, math.Copysign(0, -1), 1e300, 1e-300}[g.r.IntN(5)]
			if g.r.IntN(3) == 0 {
				x = math.SmallestNonzeroFloat64 * float64(1+g.r.IntN(8))
			}
		}
		l.value, r.value = float(x), float(y)
	default:
		x := int64(1<<53+1+2*g.r.IntN(1<<20)) * int64(1-2*g.r.IntN(2))
		if g.r.IntN(4) == 0 {
			x = math.MaxInt64 - int64(g.r.IntN(1024))
		}
		l.value, r.value = integer(x), float(g.real())
		if g.r.IntN(2) == 0 {
			l.value, r.value = r.value, l.value
		}
	}
}

// fusable reports whether n is a sum or a difference of a product of two leaves and a leaf.
func (g *generator) fusable(n *gnode) bool {
	if n.kind != stmt.ExprAdd && n.kind != stmt.ExprSubtract {
		return false
	}
	for k, m := range n.operands {
		if m.kind == stmt.ExprMultiply && m.binary() && n.operands[1-k].leaf() {
			return true
		}
	}
	return false
}

// fuse the values of n, a sum or a difference of a product of the reals x and y and a leaf z, so
// that the result is the rounded product minus itself, zero, or the next real after it, where a
// fused instruction, which does not round the product, gives the error of its rounding.
func (g *generator) fuse(n *gnode) {
	k := 0
	if n.operands[1].kind == stmt.ExprMultiply {
		k = 1
	}
	m, z := n.operands[k], n.operands[1-k]
	x := g.r.NormFloat64() * math.Pow(10, float64(g.r.IntN(7)-3))
	y := g.r.NormFloat64() * float64(1+g.r.IntN(1000))
	p := float64(x * y)
	if n.kind == stmt.ExprAdd {
		p = -p
	}
	if g.r.IntN(4) == 0 {
		p = math.Nextafter(p, math.Inf(1))
	}
	m.operands[0].value, m.operands[1].value, z.value = float(x), float(y), float(p)
}

// zero the binary operator n of two leaves: equal values for a difference, opposite ones for a sum,
// and a zero on the left of a product or a quotient.
func (g *generator) zero(n *gnode) {
	l, r := n.operands[0], n.operands[1]
	v := g.number()
	switch n.kind {
	case stmt.ExprSubtract:
		l.value, r.value = v, v
	case stmt.ExprAdd:
		l.value = v
		if i, ok := valueOf(v).Integer(); ok && i != math.MinInt64 {
			r.value = integer(-i)
		} else if f, ok := valueOf(v).Real(); ok {
			r.value = float(-f)
		}
	default:
		l.value = []*s4qlpb.Value{integer(0), float(0), float(math.Copysign(0, -1))}[g.r.IntN(3)]
	}
}

// fill the leaves under n that have no value yet: NULL one time in twelve, and otherwise an integer
// or a real.
func (g *generator) fill(n *gnode) {
	if n.leaf() && n.value == nil {
		n.value = g.number()
		if g.r.IntN(12) == 0 {
			n.value = null()
		}
	}
	for _, o := range n.operands {
		g.fill(o)
	}
}

// plant a text or a blob in a leaf under n at random.
func (g *generator) plant(n *gnode) {
	var leaves []*gnode
	var walk func(n *gnode)
	walk = func(n *gnode) {
		if n.leaf() {
			leaves = append(leaves, n)
		}
		for _, o := range n.operands {
			walk(o)
		}
	}
	walk(n)
	leaves[g.r.IntN(len(leaves))].value = []*s4qlpb.Value{text("1"), blob([]byte("1"))}[g.r.IntN(2)]
}

// number at random: an integer or a real, as often.
func (g *generator) number() *s4qlpb.Value {
	if g.r.IntN(2) == 0 {
		return integer(g.integer())
	}
	return float(g.real())
}

// integer at random: one of [stmttest.Integer], a small one, or an edge of the conversion to a real
// or of a product.
func (g *generator) integer() int64 {
	switch g.r.IntN(4) {
	case 0, 1:
		return stmttest.Integer(g.r)
	case 2:
		return int64(g.r.IntN(21) - 10)
	}
	return []int64{1 << 53, 1<<53 + 1, -(1<<53 + 1), 1<<53 - 1, 1 << 62, 3037000499, -3037000500, 1 << 31, -1 << 32,
		math.MaxInt64 / 2, math.MinInt64 / 2}[g.r.IntN(11)]
}

// real at random, finite: one of [stmttest.Real], an edge, or a number between -1000 and 1000 with
// every bit of its fraction drawn.
func (g *generator) real() float64 {
	switch g.r.IntN(4) {
	case 0, 1:
		return stmttest.Real(g.r)
	case 2:
		return []float64{0, math.Copysign(0, -1), 0.1, 0.5, 1.5, 3, -2.5, 1e308, -1e308, 0x1p53, 0x1p53 + 2, 1e-308, 1e16,
			0x1p63, -0x1p63, math.SmallestNonzeroFloat64, 1.0 / 3, math.MaxFloat64}[g.r.IntN(18)]
	}
	return (g.r.Float64() - 0.5) * 2000
}

// columnValue at random for the column at the position i of [schema], which no leaf took: NULL one
// time in eight, or a short value of its type.
func (g *generator) columnValue(i int) *s4qlpb.Value {
	switch {
	case g.r.IntN(8) == 0:
		return null()
	case schema[i].typ == s4qlpb.Type_TYPE_TEXT:
		return text(g.textOf(g.shortLength()))
	case schema[i].typ == s4qlpb.Type_TYPE_BLOB:
		return blob(g.blobOf(g.shortLength()))
	case schema[i].typ == s4qlpb.Type_TYPE_REAL:
		return float(g.real())
	}
	return integer(g.integer())
}

// precedenceOf the operator of the kind k, higher for one that binds more tightly, as in SQLite, and
// 0 for no operator.
func precedenceOf(k stmt.ExpressionKind) int {
	switch k {
	case 0:
		return 0
	case stmt.ExprNegate:
		return 3
	case stmt.ExprMultiply, stmt.ExprDivide:
		return 2
	}
	return 1
}

// write the text of n, an operand of the operator parent, or the root for 0, on its right if right,
// with parentheses where SQLite needs them, and one time in five where it does not.
func (g *generator) write(n *gnode, parent stmt.ExpressionKind, right bool) {
	if n.leaf() {
		g.leaf(n.value, parent == stmt.ExprDivide && right, false)
		return
	}
	p, outer := precedenceOf(n.kind), precedenceOf(parent)
	paren := outer > p || right && outer == p || outer > 0 && g.r.IntN(5) == 0
	if paren {
		g.b.WriteString("(")
	}
	if n.kind == stmt.ExprNegate {
		g.b.WriteString("-")
		if o := n.operands[0]; o.leaf() {
			g.leaf(o.value, false, true)
		} else {
			// A negation of a negation, or of an operator, needs the parentheses: SQLite's - before
			// a - is a second sign, which the pass refuses.
			g.b.WriteString("(")
			g.write(o, 0, false)
			g.b.WriteString(")")
		}
	} else {
		g.write(n.operands[0], n.kind, false)
		g.b.WriteString(" " + n.kind.String() + " ")
		g.write(n.operands[1], n.kind, true)
	}
	if paren {
		g.b.WriteString(")")
	}
}

// leaf of the value v: a literal two times in five, a column two times in five, and a parameter
// otherwise, as v allows. A literal is a number, never one after a negation, which would be a
// negative literal, and never a divisor that is zero or that SQLite may read as zero, which the
// pass refuses. A column must be free, or have v.
func (g *generator) leaf(v *s4qlpb.Value, divisor, negated bool) {
	literal := !negated
	switch f := valueOf(v); {
	case v.HasText(), v.HasBlob(), v.HasNull():
		literal = false
	case divisor:
		r, isReal := f.Real()
		i, _ := f.Integer()
		literal = literal && (isReal && math.Abs(r) > 1e-300 || !isReal && i != 0)
	}
	switch n := g.r.IntN(5); {
	case n < 2 && literal:
		g.literal(v)
	case n < 4 && g.column(v):
	default:
		g.b.WriteString("?")
		g.args = append(g.args, v)
	}
}

// literal of v, an integer or a real, as [stmttest.Literal] writes it.
func (g *generator) literal(v *s4qlpb.Value) {
	var text string
	if v.HasInteger() {
		text, _ = stmttest.Literal(g.r, stmt.Integer(v.GetInteger()))
	} else {
		text, _ = stmttest.Literal(g.r, stmt.Real(v.GetReal()))
	}
	g.b.WriteString(text)
}

// column of v: the name of a column of [schema] of its type, or for NULL any of the types that
// [generator.nullIn] names, that is free or has v, which it takes, in lower case, upper case, or
// double quotes; and whether it wrote one.
func (g *generator) column(v *s4qlpb.Value) bool {
	var candidates []int
	for i, c := range schema {
		var fits bool
		switch c.typ {
		case s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INTEGER:
			fits = v.HasInteger() || v.HasNull() && g.nullIn == nullInNumber
		case s4qlpb.Type_TYPE_REAL:
			fits = v.HasReal() || v.HasNull() && g.nullIn == nullInNumber
		case s4qlpb.Type_TYPE_TEXT:
			fits = v.HasText() || v.HasNull() && g.nullIn != nullInNumber
		case s4qlpb.Type_TYPE_BLOB:
			fits = v.HasBlob() || v.HasNull() && g.nullIn == nullInTextOrBlob
		}
		if fits && (g.row[i] == nil || valueOf(g.row[i]) == valueOf(v)) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	i := candidates[g.r.IntN(len(candidates))]
	if g.row[i] == nil {
		g.row[i] = v
	}
	name := schema[i].name
	switch g.r.IntN(6) {
	case 0:
		name = strings.ToUpper(name)
	case 1:
		name = `"` + name + `"`
	}
	g.b.WriteString(name)
	return true
}

// textCase of the generator: a tree of ||, lower, and hex of 1 to 5 levels, whose leaves are
// literals, columns, and parameters of text, of blobs as the argument of hex, and NULL from a
// column or a parameter one leaf in ten, with text that is empty, ASCII of every kind, and not
// ASCII. One case in four has a length limit from 1,000 to 4,000 bytes, or as long as SQLite needs
// for the text of its statement; three times in four but in a deep tree, its values have lengths
// that put an operation on either side of the limit. One case in 30 is a deep tree, a chain of || as
// deep as the pass takes or calls nested in 100 parentheses, without NULL, which would make every
// operation above it NULL; and one in 40 is a column alone. One case in 25 has a number, or a blob
// where text is due, from a column or an argument.
func (g *generator) textCase() exprCase {
	if g.r.IntN(4) == 0 {
		g.limit = 1000 + g.r.IntN(3001)
	}
	var root *gnode
	switch {
	case g.r.IntN(40) == 0:
		root = &gnode{}
	case g.r.IntN(30) == 0:
		root, g.deep = g.deepTree(), true
	default:
		root = g.textOperator(1 + g.r.IntN(5))
	}
	if g.limit > 0 && !root.leaf() && !g.deep && g.r.IntN(4) > 0 {
		g.edgeOfLimit(root)
		if g.r.IntN(4) == 0 {
			// NULL beside an operation at the edge of the limit, whose error comes all the same.
			root = &gnode{kind: stmt.ExprConcat, operands: []*gnode{{value: null()}, root}}
		}
	}
	g.fillText(root, 0, 0)
	if g.r.IntN(25) == 0 {
		g.plantNumber(root)
	}
	if root.leaf() {
		g.nullIn = nullInTextOrBlob
		if root.value.HasInteger() || root.value.HasReal() {
			g.nullIn = nullInNumber
		}
		// Every column is free, so one takes the value.
		g.column(root.value)
	} else {
		g.writeText(root, false, false, false)
	}
	return g.done()
}

// textOperator of at most depth levels: a concatenation one time in two, and a call of lower or of
// hex otherwise.
func (g *generator) textOperator(depth int) *gnode {
	shape := func() *gnode {
		if depth == 1 || g.r.IntN(10) < 3 {
			return &gnode{}
		}
		return g.textOperator(depth - 1)
	}
	switch g.r.IntN(4) {
	case 0, 1:
		return &gnode{kind: stmt.ExprConcat, operands: []*gnode{shape(), shape()}}
	case 2:
		return &gnode{kind: stmt.ExprCall, function: stmt.FuncLower, operands: []*gnode{shape()}}
	}
	return &gnode{kind: stmt.ExprCall, function: stmt.FuncHex, operands: []*gnode{shape()}}
}

// deepTree: one time in two a chain of || to the left of 900 to 997 operators, whose operands are
// leaves or calls of a leaf, which is as deep as the pass takes; and otherwise calls nested 90 to
// 100 deep, of which at most three are of hex, under a chain of || of up to 889 operators.
func (g *generator) deepTree() *gnode {
	call := func(o *gnode, f stmt.Function) *gnode {
		return &gnode{kind: stmt.ExprCall, function: f, operands: []*gnode{o}}
	}
	if g.r.IntN(2) == 0 {
		n := &gnode{}
		hexes := 0
		for range 90 + g.r.IntN(11) {
			f := stmt.FuncLower
			if hexes < 3 && g.r.IntN(20) == 0 {
				f = stmt.FuncHex
				hexes++
			}
			n = call(n, f)
		}
		for range g.r.IntN(890) {
			n = &gnode{kind: stmt.ExprConcat, operands: []*gnode{n, {}}}
		}
		return n
	}
	operand := func() *gnode {
		if g.r.IntN(4) == 0 {
			return call(&gnode{}, []stmt.Function{stmt.FuncLower, stmt.FuncHex}[g.r.IntN(2)])
		}
		return &gnode{}
	}
	n := operand()
	for range 900 + g.r.IntN(98) {
		n = &gnode{kind: stmt.ExprConcat, operands: []*gnode{n, operand()}}
	}
	return n
}

// edgeOfLimit of the tree under n, which has a length limit: lengths of the leaves under an
// operation, two times in three the root, such that it makes a text at the limit or next to it. A
// concatenation is one byte below the limit, at it, or one above it; lower takes a text one byte
// below the limit, of which SQLite makes one, or one at it, of which SQLite makes none; and hex takes
// the longest text that SQLite makes one from, or one byte more.
func (g *generator) edgeOfLimit(n *gnode) {
	for g.r.IntN(3) == 0 {
		o := n.operands[g.r.IntN(len(n.operands))]
		if o.leaf() {
			break
		}
		n = o
	}
	l := g.limit
	switch {
	case n.kind == stmt.ExprConcat:
		g.lengthOf(n, l-1+g.r.IntN(3))
	case n.function == stmt.FuncLower:
		g.lengthOf(n.operands[0], l-1+g.r.IntN(2))
	default:
		g.lengthOf(n.operands[0], (l-1)/2+g.r.IntN(2))
	}
}

// lengthOf the text of n, which its leaves are given as integers, which [generator.fillText] reads,
// as near as the tree allows: a concatenation splits it between its operands at random, lower passes
// it to its argument, and hex passes half of it, rounded down, to its argument.
func (g *generator) lengthOf(n *gnode, length int) {
	length = max(0, length)
	switch {
	case n.leaf():
		if g.r.IntN(30) == 0 {
			// A literal longer than the limit, as [generator.textLeaf] writes it.
			length = g.limit + 1 + g.r.IntN(2)
		}
		n.value = integer(int64(length))
	case n.kind == stmt.ExprConcat:
		left := g.r.IntN(length + 1)
		g.lengthOf(n.operands[0], left)
		g.lengthOf(n.operands[1], length-left)
	case n.function == stmt.FuncLower:
		g.lengthOf(n.operands[0], length)
	default:
		g.lengthOf(n.operands[0], length/2)
	}
}

// fillText gives each leaf under n, the operand of an operator of the kind parent and the function
// f, a value: a text, or as the argument of hex or a column alone a blob one time in three, of the
// length that [generator.lengthOf] gave it, or of a short one; or NULL one time in ten where no
// length was given, but in a deep tree.
func (g *generator) fillText(n *gnode, parent stmt.ExpressionKind, f stmt.Function) {
	if !n.leaf() {
		for _, o := range n.operands {
			g.fillText(o, n.kind, n.function)
		}
		return
	}
	length := g.shortLength()
	if n.value.HasNull() {
		return
	}
	if n.value != nil {
		length = int(n.value.GetInteger())
	} else if !g.deep && g.r.IntN(10) == 0 {
		n.value = null()
		return
	}
	if (f == stmt.FuncHex || parent == 0) && g.r.IntN(3) == 0 {
		n.value = blob(g.blobOf(length))
		return
	}
	n.value = text(g.textOf(length))
}

// plantNumber in a leaf under n at random: an integer or a real, or a blob where text is due.
func (g *generator) plantNumber(n *gnode) {
	type leaf struct {
		n     *gnode
		inHex bool
	}
	var leaves []leaf
	var walk func(n *gnode, inHex bool)
	walk = func(n *gnode, inHex bool) {
		if n.leaf() {
			leaves = append(leaves, leaf{n, inHex})
		}
		for _, o := range n.operands {
			walk(o, n.function == stmt.FuncHex)
		}
	}
	walk(n, false)
	l := leaves[g.r.IntN(len(leaves))]
	l.n.value = g.number()
	if !l.inHex && g.r.IntN(3) == 0 {
		l.n.value = blob(g.blobOf(g.shortLength()))
	}
}

// shortLength of a text or a blob at random: 0 one time in six, and otherwise from 1 to 12.
func (g *generator) shortLength() int {
	if g.r.IntN(6) == 0 {
		return 0
	}
	return 1 + g.r.IntN(12)
}

// textOf the length at random, of valid UTF-8 without the character NUL, of pieces: ASCII letters,
// the bytes next to them, which lower must not change, a quote, which a literal doubles, and
// letters that are not ASCII, which SQLite's lower leaves as they are, also where Unicode has a
// lower case of its own, as for the Kelvin sign and the dotted capital I. The first ten pieces are
// one byte long.
func (g *generator) textOf(length int) string {
	pieces := []string{"A", "M", "Z", "a", "z", "@", "[", "`", "{", "'", "À", "É", "é", "ß", "Ω", "İ", "\u212a", "€", "😀"}
	var b strings.Builder
	for b.Len() < length {
		p := pieces[g.r.IntN(len(pieces))]
		if b.Len()+len(p) > length {
			p = pieces[g.r.IntN(10)]
		}
		b.WriteString(p)
	}
	return b.String()
}

// blobOf the length at random, of bytes of every value, of the ASCII capital letters often.
func (g *generator) blobOf(length int) []byte {
	b := make([]byte, length)
	for i := range b {
		b[i] = byte(g.r.IntN(256))
		if g.r.IntN(3) == 0 {
			b[i] = byte('A' + g.r.IntN(26))
		}
	}
	return b
}

// writeText of n, a tree of text, an operand of || if inConcat, the right one if right, and in the
// argument of hex if inHex, with parentheses where SQLite needs them, around a concatenation on the
// right of ||, and one time in five around one on the left but in a deep tree; the pass takes no
// others.
func (g *generator) writeText(n *gnode, inConcat, right, inHex bool) {
	switch {
	case n.leaf():
		g.textLeaf(n.value, inHex)
	case n.kind == stmt.ExprCall:
		name := n.function.String()
		if g.r.IntN(2) == 0 {
			name = strings.ToUpper(name)
		}
		g.b.WriteString(name + "(")
		g.writeText(n.operands[0], false, false, n.function == stmt.FuncHex)
		g.b.WriteString(")")
	default:
		paren := inConcat && (right || !g.deep && g.r.IntN(5) == 0)
		if paren {
			g.b.WriteString("(")
		}
		g.writeText(n.operands[0], true, false, false)
		g.b.WriteString(" || ")
		g.writeText(n.operands[1], true, true, false)
		if paren {
			g.b.WriteString(")")
		}
	}
}

// textLeaf of the value v, in the argument of hex if inHex: a literal two times in five, a column
// two times in five, and a parameter otherwise, as v allows. A literal is a text, or a blob in the
// argument of hex, and never a number or NULL, which the pass refuses; it is at most 40 bytes long,
// or with a limit one time in eight as long as the limit allows. A value longer than the limit is
// always a literal, since SQLite refuses to bind it.
func (g *generator) textLeaf(v *s4qlpb.Value, inHex bool) {
	length := len(v.GetText()) + len(v.GetBlob())
	literal := (v.HasText() || inHex && v.HasBlob()) && (length <= 40 || g.limit > 0 && g.r.IntN(8) == 0)
	if g.limit > 0 && length > g.limit {
		literal = true
	}
	g.nullIn = nullInText
	if inHex {
		g.nullIn = nullInTextOrBlob
	}
	defer func() {
		g.nullIn = nullInNumber
	}()
	switch n := g.r.IntN(5); {
	case literal && (n < 2 || g.limit > 0 && length > g.limit) && v.HasText():
		g.b.WriteString("'" + strings.ReplaceAll(v.GetText(), "'", "''") + "'")
	case literal && (n < 2 || g.limit > 0 && length > g.limit):
		format := []string{"x'%x'", "X'%X'"}[g.r.IntN(2)]
		fmt.Fprintf(&g.b, format, v.GetBlob())
	case n < 4 && g.column(v):
	default:
		g.b.WriteString("?")
		g.args = append(g.args, v)
	}
}
