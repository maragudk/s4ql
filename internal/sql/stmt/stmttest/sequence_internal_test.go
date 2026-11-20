package stmttest

import (
	"bytes"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestSequence_neighbour(t *testing.T) {
	t.Run("should give a value of the same type next to the value, which a column of the type holds", func(t *testing.T) {
		q, err := NewSequence(rand.New(rand.NewPCG(1, 2)))
		is.NotError(t, err)
		r := rand.New(rand.NewPCG(3, 4))
		for range 5000 {
			for _, typ := range Types {
				v := ValueOf(r, typ)
				if r.IntN(4) == 0 {
					v = edgeOf(r, typ)
				}
				n := q.neighbour(v)
				switch v := v.(type) {
				case stmt.Integer:
					d := int64(n.(stmt.Integer)) - int64(v)
					is.True(t, d == 1 || d == -1, "an integer one more or one less")
				case stmt.Real:
					f, g := float64(v), float64(n.(stmt.Real))
					is.True(t, !math.IsInf(g, 0) && !math.IsNaN(g), "a finite real")
					is.True(t, f == g && math.Abs(f) == math.MaxFloat64 || math.Nextafter(f, g) == g, "the next real above or below")
				case stmt.Text:
					s := string(n.(stmt.Text))
					is.True(t, utf8.ValidString(s) && !strings.ContainsRune(s, 0), "text that a column holds")
					last, _ := utf8.DecodeLastRuneInString(string(v))
					is.True(t, s != string(v) || last == 1 || last == 0xd7ff || last == 0xe000 || last == utf8.MaxRune, "another text")
				case stmt.Blob:
					b := n.(stmt.Blob)
					is.True(t, b != nil, "a blob that is not nil")
					is.True(t, !bytes.Equal(b, v) || len(v) > 0 && (v[len(v)-1] == 0 || v[len(v)-1] == 0xff), "another blob")
				}
			}
		}
	})
}

func TestSequence_where(t *testing.T) {
	t.Run("should give conditions on the columns of the shape and on others, with values of the inserts, and the forms of slice 7", func(t *testing.T) {
		q, err := NewSequence(rand.New(rand.NewPCG(5, 6)))
		is.NotError(t, err)
		table := stmt.Name{Raw: "t", Value: "t"}
		shape := q.shapeOf(table)
		for range 50 {
			q.insert(table)
		}
		counts := map[string]int{}
		for range 4000 {
			cs := q.where(table)
			if cs == nil {
				counts["no WHERE"]++
				continue
			}
			for i, c := range cs {
				column := columnOfShape(shape, nameOf(c.Column.Raw, 0).Fold())
				switch {
				case column == nil && c.Column.Raw[0] == '"':
					counts["a column that the shape does not have, in double quotes"]++
				case column == nil:
					counts["a column that the shape does not have"]++
				case column.PrimaryKey:
					counts["the key"]++
				default:
					counts["another column"]++
				}
				if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull {
					is.True(t, c.Value.Value == nil, "a test for NULL without a value")
					counts["a test for NULL"]++
					continue
				}
				is.True(t, c.Value.Value != nil, "a comparison with a value")
				if _, ok := c.Value.Value.(stmt.Null); ok {
					counts["NULL"]++
				}
				if column != nil {
					for _, v := range q.stored[[2]string{"t", column.Name.Value}] {
						if valueKey(v) == valueKey(c.Value.Value) {
							counts["a value of an insert"]++
							break
						}
					}
				}
				for _, d := range cs[:i] {
					if d.Column.Raw == c.Column.Raw && inverses[d.Operator] == c.Operator && valueKey(d.Value.Value) == valueKey(c.Value.Value) {
						counts["the inverse of an earlier condition"]++
					}
				}
			}
		}
		t.Log(counts)
		is.True(t, counts["no WHERE"] > 1800 && counts["no WHERE"] < 2200, "one select in two without WHERE")
		for _, kind := range []string{"a column that the shape does not have, in double quotes", "a column that the shape does not have", "the key", "another column",
			"a test for NULL", "NULL", "a value of an insert", "the inverse of an earlier condition"} {
			is.True(t, counts[kind] >= 50, kind)
		}
	})
}

// valueKey of a value, for a comparison of two in a test.
func valueKey(v stmt.Value) string {
	text, _ := Literal(rand.New(rand.NewPCG(0, 0)), v)
	return text
}
