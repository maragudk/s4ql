package expr_test

import (
	"math"
	"math/big"
	"math/rand/v2"
	"strings"

	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

// generator of cases at random: trees of +, -, *, /, and negations, of varying depth and shape,
// whose leaves are literals, columns, and parameters with integers, reals, and NULL that weigh the
// edges, and often with values that are related so that an operation reaches its edge: a sum, a
// difference, a product, or a quotient just inside the integers of 64 bits and just outside, a
// divisor that is zero or an operation that gives zero under a division, reals whose operation is
// infinite or underflows, a product whose sum or difference is zero or next to it where a fused
// instruction would give another result, and integers above 2^53 with reals. One case in 25 has a
// text or a blob as an operand, from a column or an argument, and one in 40 is a column alone.
type generator struct {
	r *rand.Rand
	b strings.Builder
	// row of the case, with nil for a column that no leaf has taken yet.
	row  []*s4qlpb.Value
	args []*s4qlpb.Value
}

// newGenerator of the seed.
func newGenerator(seed uint64) *generator {
	return &generator{r: rand.New(rand.NewPCG(seed, 0x5e4a))}
}

// gnode of a tree of the [generator]: an operator, or a leaf, of no kind, whose text, a literal, a
// column, or a parameter, is chosen when it is written, after its value.
type gnode struct {
	kind     stmt.ExpressionKind
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
	for i, v := range g.row {
		if v == nil {
			g.row[i] = g.columnValue(i)
		}
	}
	return exprCase{src: g.b.String(), row: g.row, args: g.args}
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
// time in eight, or a value of its type.
func (g *generator) columnValue(i int) *s4qlpb.Value {
	switch {
	case schema[i].typ == s4qlpb.Type_TYPE_TEXT:
		return text("4")
	case schema[i].typ == s4qlpb.Type_TYPE_BLOB:
		return blob([]byte("5"))
	case g.r.IntN(8) == 0:
		return null()
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

// column of v: the name of a column of [schema] of its type, or any of a number for NULL, that is
// free or has v, which it takes, in lower case, upper case, or double quotes; and whether it wrote
// one.
func (g *generator) column(v *s4qlpb.Value) bool {
	var candidates []int
	for i, c := range schema {
		var fits bool
		switch c.typ {
		case s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INTEGER:
			fits = v.HasInteger() || v.HasNull()
		case s4qlpb.Type_TYPE_REAL:
			fits = v.HasReal() || v.HasNull()
		case s4qlpb.Type_TYPE_TEXT:
			fits = v.HasText()
		case s4qlpb.Type_TYPE_BLOB:
			fits = v.HasBlob()
		}
		if fits && (g.row[i] == nil || v.HasText() || v.HasBlob() || valueOf(g.row[i]) == valueOf(v)) {
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
