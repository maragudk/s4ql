package stmttest

import (
	"errors"
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestSequence_text(t *testing.T) {
	// countsOf the statements with text of 10 sequences of the profile p, which must each be the
	// statement that the pass gives for its text, by what their text has and where.
	countsOf := func(t *testing.T, p *Profile) map[string]int {
		counts := map[string]int{}
		count := func(e stmt.Expression, where string) {
			if !isText(e) {
				return
			}
			counts[map[bool]string{true: "a text with a column in ", false: "a text without a column in "}[e.HasColumn()]+where]++
			var walk func(e stmt.Expression)
			walk = func(e stmt.Expression) {
				node := "a node " + e.Kind.String()
				if e.Function != 0 {
					node += " " + e.Function.String()
				}
				counts[node]++
				for _, o := range e.Operands {
					walk(o)
				}
			}
			walk(e)
		}
		for k := range uint64(10) {
			q, err := newSequence(p.Seed(k), p)
			is.NotError(t, err)
			for range 200 {
				c := q.Next()
				s, err := stmt.Check(c.Src)
				switch {
				case c.Supported && err == nil && s != nil && hasText(s):
					counts["a call with text"]++
				case !c.Supported && errors.Is(err, stmt.ErrNotSupported):
					counts["a text that the pass refuses"]++
				}
			}
			if q.textAgain == nil {
				continue
			}
			for i := range 300 {
				q.step++
				q.inner = nil
				kind := []Kind{KindSelect, KindUpdate, KindDelete}[i%3]
				src, s, args := q.g.CallOf(kind)
				got, err := stmt.Check(src)
				is.NotError(t, err, src)
				is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
				is.Equal(t, got.Parameters(), len(args), src)
				if !hasText(got) {
					continue
				}
				counts["a statement with text"]++
				if u, ok := got.(*stmt.Update); ok {
					for _, a := range u.Set {
						count(a.Value.Expression, "SET")
					}
				}
				where, subs := whereOf(got)
				for _, sub := range subs {
					for _, c := range sub.Where {
						count(c.Expression, "a subquery")
						count(c.Value.Expression, "a subquery")
					}
				}
				for _, c := range where {
					count(c.Expression, "a tested side")
					count(c.Value.Expression, "a value")
					if isText(c.Expression) && c.Value.Subquery > 0 {
						counts["a tested text with a subquery"]++
					}
					if isText(c.Expression) && !c.Value.Expression.Present() && c.Value.Subquery == 0 {
						counts["a tested text with a plain value"]++
					}
					if c.Expression.Kind != stmt.ExprColumn && c.Expression.Present() && !isText(c.Expression) || isArithmetic(c.Value.Expression) {
						counts["arithmetic beside text"]++
					}
				}
				for _, a := range args {
					switch a := a.(type) {
					case nil:
						counts["an argument NULL"]++
					case string:
						if len(a) >= LengthLimit-1 {
							counts["an argument at the length limit"]++
						}
					case []byte:
						counts["an argument that is a blob"]++
					}
				}
			}
		}
		t.Log(counts)
		return counts
	}

	t.Run("should write text in SET and on either side of a comparison, which the pass reads as the generator typed it", func(t *testing.T) {
		counts := countsOf(t, TextFunctions)
		for _, kind := range []string{"a statement with text", "a call with text", "a text that the pass refuses",
			"a text with a column in SET", "a text without a column in SET", "a text with a column in a value", "a text without a column in a value",
			"a text with a column in a tested side", "a text with a column in a subquery", "a text without a column in a subquery",
			"a node ||", "a node call lower", "a node call hex", "a node parameter", "a node value", "a node column",
			"a tested text with a plain value", "a tested text with a subquery", "arithmetic beside text",
			"an argument NULL", "an argument at the length limit", "an argument that is a blob"} {
			is.True(t, counts[kind] >= 10, kind)
		}
	})

	t.Run("should write text in mixed too", func(t *testing.T) {
		counts := countsOf(t, Mixed)
		for _, kind := range []string{"a statement with text", "a text with a column in a tested side", "a text without a column in a value"} {
			is.True(t, counts[kind] >= 3, kind)
		}
	})

	t.Run("should refuse each text of TextRefused in the pass", func(t *testing.T) {
		for _, form := range TextRefused {
			src := fmt.Sprintf(form, "t", "a")
			_, err := stmt.Check(src)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), src)
		}
	})

	t.Run("should record each edge of the length limit that a statement has a text written for, and none for a text that runs again", func(t *testing.T) {
		counts := map[TextEdge]int{}
		for k := range uint64(10) {
			q, err := newSequence(TextFunctions.Seed(k), TextFunctions)
			is.NotError(t, err)
			var last *Call
			for range 300 {
				c := q.Next()
				if last != nil && c.Src == last.Src && c.Supported {
					// The text that runs again, which the area draws before any statement.
					is.Equal(t, 0, len(c.TextEdges), c.Src)
				}
				for _, e := range c.TextEdges {
					counts[e]++
				}
				if c.Supported {
					last = &c
				}
			}
		}
		// The longest result of || that SQLite gives under the limit, and the longest arguments of lower
		// and of hex, each written out here and not taken from edgeLength.
		at := map[stmt.Function]int{0: LengthLimit, stmt.FuncLower: LengthLimit - 1, stmt.FuncHex: (LengthLimit - 1) / 2}
		for f, length := range at {
			for _, delta := range []int{-1, 0, 1} {
				e := TextEdge{Function: f, Length: length + delta}
				is.True(t, counts[e] >= 3, fmt.Sprint(e))
			}
		}
		for e := range counts {
			// Each a byte short of an edge, at it, or a byte over it.
			is.True(t, e.Length >= at[e.Function]-1 && e.Length <= at[e.Function]+1, fmt.Sprint(e))
		}
	})

	t.Run("should give a key of type TEXT to some tables of the profile", func(t *testing.T) {
		texts := 0
		for k := range uint64(20) {
			q, err := newSequence(TextFunctions.Seed(k), TextFunctions)
			is.NotError(t, err)
			for _, raw := range q.tables {
				for _, c := range q.shapeOf(nameOf(raw, 0)) {
					if c.PrimaryKey && c.Type == stmt.TypeText {
						texts++
					}
				}
			}
		}
		is.True(t, texts >= 20, "keys of type TEXT")
	})
}
