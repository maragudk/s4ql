package shapetest_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sql/shapetest"
)

// parse src with the parser, as a result for the harness of internal/sql/parsetest.
func parse(src string) parsetest.Result {
	_, tail, err := parser.Parse(src)
	if err != nil {
		var e *parser.Error
		if !errors.As(err, &e) {
			panic(err)
		}
		return parsetest.Result{Message: e.Message, Offset: e.Offset}
	}
	return parsetest.Result{OK: true, Offset: -1, Tail: tail}
}

// how a generated text, or a combination of forms, was compared.
type how struct {
	rejected, both, bytecode, values, neither int
}

func (h *how) add(r shapetest.Result) {
	if r.NotChecked() {
		h.rejected++
		return
	}
	byValues, byBytecode := false, r.Way == shapetest.Bytecode
	for _, e := range r.Exprs {
		byBytecode = byBytecode || e.Bytecode
		if e.Values > 0 {
			byValues = true
		}
	}
	switch {
	case byBytecode && byValues:
		h.both++
	case byBytecode:
		h.bytecode++
	case byValues:
		h.values++
	default:
		h.neither++
	}
}

// summary of how generated texts were compared, by text and by combination of forms: a combination
// counts as compared by bytecode, by values, or by both if any of its texts was.
func summary(name string, gs []shapetest.Generated, results []shapetest.Result) string {
	var texts how
	combos := map[string]*how{}
	for i, g := range gs {
		texts.add(results[i])
		key := strings.Join(g.Forms, " / ")
		if combos[key] == nil {
			combos[key] = &how{}
		}
		combos[key].add(results[i])
	}
	var byCombo how
	var neither []string
	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for k := range combos {
			if !yield(k) {
				return
			}
		}
	}) {
		h := combos[key]
		switch {
		case h.both > 0 || (h.bytecode > 0 && h.values > 0):
			byCombo.both++
		case h.bytecode > 0:
			byCombo.bytecode++
		case h.values > 0:
			byCombo.values++
		case h.neither > 0:
			byCombo.neither++
			neither = append(neither, key)
		default:
			byCombo.rejected++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d texts: %d rejected by SQLite and the parser, %d compared by bytecode and values, %d by bytecode only, %d by values only, %d only by the same error\n",
		name, len(gs), texts.rejected, texts.both, texts.bytecode, texts.values, texts.neither)
	fmt.Fprintf(&b, "%s: %d combinations: %d always rejected, %d compared by bytecode and values, %d by bytecode only, %d by values only, %d only by the same error\n",
		name, len(combos), byCombo.rejected, byCombo.both, byCombo.bytecode, byCombo.values, byCombo.neither)
	if len(neither) > 0 && len(neither) <= 400 {
		fmt.Fprintf(&b, "  only by the same error: %s\n", strings.Join(neither, ", "))
	}
	return b.String()
}

// TestCheck_pairs checks every ordered pair of operator forms, with the second in each operand of
// the first and no parentheses, and a sample of triples: the parser must group them as SQLite
// does. Texts that SQLite rejects must be rejected by the parser too.
func TestCheck_pairs(t *testing.T) {
	n, triples := 3, 20000
	if slow() {
		n, triples = 1, 2000
	}
	gs := append(shapetest.Pairs(1, n), shapetest.Triples(2, triples)...)
	var sqls []string
	for _, g := range gs {
		sqls = append(sqls, g.SQL)
	}

	r, results, ok := checkShardedResults(t, sqls)
	if !ok {
		return
	}
	t.Log("\n" + r.String())
	is.Equal(t, 0, len(r.Faults))

	pr := parsetest.Run(parsetest.ParserFunc(parse), sqls)
	is.Equal(t, 0, pr.Disagreements(), pr.String())

	pairs := len(gs) - triples
	t.Logf("%d forms, %d ordered pairs\n%s%s", len(shapetest.Forms), len(shapetest.Forms)*len(shapetest.Forms),
		summary("pairs", gs[:pairs], results[:pairs]), summary("triples", gs[pairs:], results[pairs:]))
}
