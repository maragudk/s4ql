package expr_test

import (
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"hegel.dev/go/hegel"

	"maragu.dev/s4ql/internal/s4qlpb"
)

// The property-based checks of the evaluator with Hegel (hegel.dev). Each draws a case and checks it
// against SQLite and [reference] by [checker.check], as [TestDifferential] does. Where a case fails,
// Hegel shrinks it to a small one before it reports it, and it reports the draws of the case.

// hegelOptions of the property-based checks: the same cases on every run and every machine, no
// directory of failing examples written beside the package, and no check of how fast a case runs,
// which depends on the machine.
func hegelOptions(cases int) []hegel.Option {
	return []hegel.Option{
		hegel.WithTestCases(cases),
		hegel.WithDerandomize(true),
		hegel.WithDatabase(""),
		hegel.SuppressHealthCheck(hegel.TooSlow),
	}
}

// checkDrawn checks the case c by [checker.check], and fails the case of Hegel with what fails. A
// case that check counts as no case is rejected, so that Hegel neither counts it nor shrinks to it.
func checkDrawn(tc hegel.TestCase, o *oracle, c exprCase) {
	k := &checker{o: o, tally: map[string]int{}}
	failure := k.check(c)
	for kind := range k.tally {
		tc.Event(kind)
		tc.Assume(!strings.HasPrefix(kind, "not a case"))
	}
	if failure != "" {
		tc.Errorf("%v", failure)
	}
}

// TestHegel_cases checks the evaluator against SQLite and [reference] with the cases that [drawCase]
// draws.
func TestHegel_cases(t *testing.T) {
	o := newOracle(t)
	hegel.Test(t, func(ht *hegel.T) {
		checkDrawn(ht, o, drawCase(ht))
	}, hegelOptions(5000)...)
}

// drawCase draws a case of the evaluator: a tree of arithmetic two times in three, and of text
// otherwise, with a length limit from 30 to 4,000 bytes one case in four, over a row of [schema]
// whose values are drawn after the limit and before the tree.
func drawCase(tc hegel.TestCase) exprCase {
	c := exprCase{row: make([]*s4qlpb.Value, len(schema))}
	if hegel.Draw(tc, hegel.WeightedBooleans(0.25)) {
		c.limit = hegel.Draw(tc, hegel.Integers(30, 4000))
	}
	for i, column := range schema {
		c.row[i] = drawColumnValue(tc, column.typ, c.limit)
	}
	w := &caseWriter{tc: tc, c: &c}
	if hegel.Draw(tc, hegel.WeightedBooleans(1.0/3)) {
		w.text(4, true)
	} else {
		w.number(4, true, false)
	}
	c.src = w.b.String()
	return c
}

// caseWriter writes the text of a case as it draws its tree, from left to right, and appends the
// argument of each parameter in the order of the text.
type caseWriter struct {
	tc hegel.TestCase
	c  *exprCase
	b  strings.Builder
}

// number writes an expression of a number of at most depth levels of operators: a leaf, one of the
// four operators, in parentheses, or the negation. The root is an operator, and an operand a leaf one
// time in three. The pass refuses parentheses around a leaf and a sign before a sign, so the operand
// of a negation, negated, is a leaf without a literal, which would be a negative literal, an
// operator, or a negation in parentheses.
func (w *caseWriter) number(depth int, root, negated bool) {
	op := 0
	if depth > 0 && (root || !hegel.Draw(w.tc, hegel.WeightedBooleans(0.35))) {
		op = hegel.Draw(w.tc, hegel.Integers(1, 5))
	}
	switch op {
	case 0:
		w.leaf(placeNumber, negated)
	case 5:
		if negated {
			w.b.WriteString("(")
		}
		w.b.WriteString("-")
		w.number(depth-1, false, true)
		if negated {
			w.b.WriteString(")")
		}
	default:
		w.b.WriteString("(")
		w.number(depth-1, false, false)
		w.b.WriteString([]string{"", " + ", " - ", " * ", " / "}[op])
		w.number(depth-1, false, false)
		w.b.WriteString(")")
	}
}

// text writes an expression of text of at most depth levels, as [caseWriter.number] does: a leaf,
// `||`, lower, or hex, whose argument is text or one time in two a blob.
func (w *caseWriter) text(depth int, root bool) {
	op := 0
	if depth > 0 && (root || !hegel.Draw(w.tc, hegel.WeightedBooleans(0.35))) {
		op = hegel.Draw(w.tc, hegel.Integers(1, 3))
	}
	switch op {
	case 0:
		w.leaf(placeText, false)
	case 1:
		w.b.WriteString("(")
		w.text(depth-1, false)
		w.b.WriteString(" || ")
		w.text(depth-1, false)
		w.b.WriteString(")")
	case 2:
		w.b.WriteString("lower(")
		w.text(depth-1, false)
		w.b.WriteString(")")
	case 3:
		w.b.WriteString("hex(")
		if hegel.Draw(w.tc, hegel.Booleans()) {
			w.leaf(placeBlob, false)
		} else {
			w.text(depth-1, false)
		}
		w.b.WriteString(")")
	}
}

// place of a leaf: where a number, a text, or a blob is due.
type place int

const (
	placeNumber place = iota
	placeText
	placeBlob
)

// leaf writes a literal, a column, or a parameter of the place p, and no literal if negated. One leaf
// in 40 is of another place, for the refusals of the types.
func (w *caseWriter) leaf(p place, negated bool) {
	if hegel.Draw(w.tc, hegel.WeightedBooleans(0.025)) {
		p = place(hegel.Draw(w.tc, hegel.Integers(0, 2)))
	}
	first := 0
	if negated {
		first = 1
	}
	switch hegel.Draw(w.tc, hegel.Integers(first, 2)) {
	case 0:
		w.b.WriteString(literalOf(drawValue(w.tc, p, false, w.c.limit)))
	case 1:
		names := map[place][]string{placeNumber: {"a", "b", "c", "d"}, placeText: {"s", "t"}, placeBlob: {"x", "y"}}[p]
		w.b.WriteString(hegel.Draw(w.tc, hegel.SampledFrom(names)))
	default:
		w.b.WriteString("?")
		w.c.args = append(w.c.args, drawValue(w.tc, p, true, w.c.limit))
	}
}

// drawColumnValue draws a value of a column of the type typ: NULL one time in seven, and otherwise
// a value of the type.
func drawColumnValue(tc hegel.TestCase, typ s4qlpb.Type, limit int) *s4qlpb.Value {
	if hegel.Draw(tc, hegel.WeightedBooleans(1.0/7)) {
		return null()
	}
	switch typ {
	case s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INTEGER:
		return integer(drawInteger(tc))
	case s4qlpb.Type_TYPE_REAL:
		return float(drawReal(tc))
	case s4qlpb.Type_TYPE_TEXT:
		return text(drawText(tc, limit))
	}
	return blob([]byte(drawBlob(tc, limit)))
}

// drawValue draws a value of the place p, an integer or a real for a number, and NULL one time in
// seven if nullable.
func drawValue(tc hegel.TestCase, p place, nullable bool, limit int) *s4qlpb.Value {
	if nullable && hegel.Draw(tc, hegel.WeightedBooleans(1.0/7)) {
		return null()
	}
	switch p {
	case placeNumber:
		if hegel.Draw(tc, hegel.Booleans()) {
			return float(drawReal(tc))
		}
		return integer(drawInteger(tc))
	case placeText:
		return text(drawText(tc, limit))
	}
	return blob([]byte(drawBlob(tc, limit)))
}

// drawInteger draws a small integer, any integer, or one at an edge of 64 bits or of the reals.
func drawInteger(tc hegel.TestCase) int64 {
	return hegel.Draw(tc, hegel.OneOf(
		hegel.Integers[int64](-4, 4),
		hegel.Integers[int64](math.MinInt64, math.MaxInt64),
		hegel.SampledFrom([]int64{math.MinInt64, math.MaxInt64, -1, 1 << 31, 1 << 32, -1 << 32, 1 << 53, 1<<53 + 1, 1 << 62, 3037000499, 3037000500}),
	))
}

// drawReal draws a finite real: any, or one at an edge of the reals.
func drawReal(tc hegel.TestCase) float64 {
	return hegel.Draw(tc, hegel.OneOf(
		hegel.Floats[float64]().AllowNaN(false).AllowInfinity(false),
		hegel.SampledFrom([]float64{0, math.Copysign(0, -1), 0.1, 1, -1, 1e308, -1e308, math.MaxFloat64, 5e-324, 1e-200, 1 << 53, 0x1p63}),
	))
}

// drawText draws a text without NUL: a short one, or one repeated up to 4,000 bytes, or with a limit,
// one time in three a text of a length at an edge of the limit, padded with capital letters, which
// lower changes.
func drawText(tc hegel.TestCase, limit int) string {
	s := hegel.Draw(tc, hegel.Text().ExcludeCharacters("\x00").MaxSize(6))
	switch {
	case limit > 0 && hegel.Draw(tc, hegel.WeightedBooleans(1.0/3)):
		n := drawEdge(tc, limit)
		for len(s) > n {
			_, size := utf8.DecodeLastRuneInString(s)
			s = s[:len(s)-size]
		}
		s += strings.Repeat("A", n-len(s))
	case s != "" && hegel.Draw(tc, hegel.WeightedBooleans(0.2)):
		s = strings.Repeat(s, hegel.Draw(tc, hegel.Integers(1, 4000/len(s))))
	}
	return s
}

// drawBlob draws a blob as [drawText] draws a text, padded with the byte 0xab.
func drawBlob(tc hegel.TestCase, limit int) string {
	b := string(hegel.Draw(tc, hegel.Binary(0, 6)))
	switch {
	case limit > 0 && hegel.Draw(tc, hegel.WeightedBooleans(1.0/3)):
		n := drawEdge(tc, limit)
		b = b[:min(len(b), n)] + strings.Repeat("\xab", max(0, n-len(b)))
	case b != "" && hegel.Draw(tc, hegel.WeightedBooleans(0.2)):
		b = strings.Repeat(b, hegel.Draw(tc, hegel.Integers(1, 2000/len(b))))
	}
	return b
}

// drawEdge draws a length at an edge of the length limit: one byte below, at, or above the limit or
// half of it, where an operation of the evaluator makes a text that is just short enough or just too
// long.
func drawEdge(tc hegel.TestCase, limit int) int {
	return hegel.Draw(tc, hegel.SampledFrom([]int{limit - 1, limit, limit + 1, limit/2 - 1, limit / 2, limit/2 + 1}))
}

// literalOf v, an integer, a real, a text, or a blob, as SQLite reads it back to v, with the sign as
// part of the literal: a real in the shortest digits that are v, which SQLite reads exactly.
func literalOf(v *s4qlpb.Value) string {
	switch {
	case v.HasInteger():
		return strconv.FormatInt(v.GetInteger(), 10)
	case v.HasReal():
		f := v.GetReal()
		s := strconv.FormatFloat(math.Abs(f), 'g', -1, 64)
		if !strings.ContainsAny(s, ".e") {
			s += ".0"
		}
		if math.Signbit(f) {
			return "-" + s
		}
		return s
	case v.HasText():
		return "'" + strings.ReplaceAll(v.GetText(), "'", "''") + "'"
	}
	return "x'" + hex.EncodeToString(v.GetBlob()) + "'"
}
