package stmttest

import (
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
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

func TestSequence_update(t *testing.T) {
	t.Run("should give assignments to the columns of the shape and to others, with values of the inserts and NULL, and a WHERE three times in four", func(t *testing.T) {
		q, err := NewSequence(rand.New(rand.NewPCG(7, 8)))
		is.NotError(t, err)
		// A table of three columns, which every seed of the shape's random numbers does not give.
		var table stmt.Name
		for i := 0; ; i++ {
			table = stmt.Name{Raw: fmt.Sprint("t", i), Value: fmt.Sprint("t", i)}
			if len(q.shapeOf(table)) == sequenceColumns {
				break
			}
		}
		shape := q.shapeOf(table)
		for range 50 {
			q.insert(table)
		}
		counts := map[string]int{}
		for range 4000 {
			set, where := q.update(table)
			is.True(t, len(set) >= 1 && len(set) <= len(shape), "one assignment to each of some columns")
			if where == nil {
				counts["no WHERE"]++
			}
			seen := map[string]bool{}
			for _, a := range set {
				fold := nameOf(a.Column.Raw, 0).Fold()
				is.True(t, !seen[fold], "no column twice")
				seen[fold] = true
				column := columnOfShape(shape, fold)
				switch {
				case column == nil:
					counts["a column that the shape does not have"]++
				case column.PrimaryKey:
					counts["the key"]++
				default:
					counts["another column"]++
				}
				is.True(t, a.Value.Value != nil, "an assignment of a value")
				if _, ok := a.Value.Value.(stmt.Null); ok {
					counts["NULL"]++
					continue
				}
				if column == nil {
					continue
				}
				if kindOf(typeOf(a.Value.Value)) != kindOf(column.Type) {
					counts["a value of another type"]++
				}
				for _, v := range q.stored[[2]string{table.Value, column.Name.Value}] {
					if valueKey(v) == valueKey(a.Value.Value) {
						counts["a value of an insert"]++
						break
					}
				}
			}
			counts[fmt.Sprint(len(set), " assignments")]++
		}
		t.Log(counts)
		is.True(t, counts["no WHERE"] > 800 && counts["no WHERE"] < 1200, "one update in four without WHERE")
		for _, kind := range []string{"a column that the shape does not have", "the key", "another column", "NULL", "a value of another type", "a value of an insert",
			"1 assignments", "2 assignments", "3 assignments"} {
			is.True(t, counts[kind] >= 50, kind)
		}
	})
}

func TestSequence_delete(t *testing.T) {
	t.Run("should give a WHERE four times in five", func(t *testing.T) {
		q, err := NewSequence(rand.New(rand.NewPCG(9, 10)))
		is.NotError(t, err)
		table := stmt.Name{Raw: "t", Value: "t"}
		for range 50 {
			q.insert(table)
		}
		none := 0
		for range 4000 {
			if q.delete(table) == nil {
				none++
			}
		}
		is.True(t, none > 650 && none < 950, "one delete in five without WHERE")
	})

	t.Run("should give, of a WHERE, one time in three the key equal to a value of an insert, or bounded on one side by every third of those values", func(t *testing.T) {
		q, err := NewSequence(rand.New(rand.NewPCG(13, 14)))
		is.NotError(t, err)
		table := stmt.Name{Raw: "t", Value: "t"}
		shape := q.shapeOf(table)
		key := shape[slices.IndexFunc(shape, func(c stmt.Column) bool { return c.PrimaryKey })]
		for range 50 {
			q.insert(table)
		}
		stored := map[string]bool{}
		for _, v := range q.stored[[2]string{table.Value, key.Name.Value}] {
			stored[valueKey(v)] = true
		}
		counts := map[stmt.Operator]int{}
		for range 4000 {
			where := q.changeConditions(table)
			if len(where) != 1 || where[0].Value.Value == nil || nameOf(where[0].Column.Raw, 0).Fold() != key.Name.Value || !stored[valueKey(where[0].Value.Value)] {
				continue
			}
			counts[where[0].Operator]++
		}
		t.Log(counts)
		is.True(t, counts[stmt.OpEqual] > 800, "the key equal to a value of an insert two times in nine")
		bounded := 0
		for _, op := range bounds {
			is.True(t, counts[op] > 80, fmt.Sprint("the key bounded by ", op))
			bounded += counts[op]
		}
		is.True(t, bounded > 400 && bounded < 800, "the key bounded on one side one time in nine, and by the conditions of a select now and then")
	})
}

func TestSequence_insert(t *testing.T) {
	t.Run("should give an integer of a column of INT or INTEGER as the real of its value one insert in eight after Mistyped", func(t *testing.T) {
		// wholeReals counts the inserts into table of the sequence q that give a column of INT or
		// INTEGER a real that is a whole number.
		wholeReals := func(q *Sequence, table stmt.Name) int {
			shape := q.shapeOf(table)
			n := 0
			for range 4000 {
				list, values := q.insert(table)
				for i, v := range values {
					var column *stmt.Column
					switch {
					case list == nil && i < len(shape):
						column = &shape[i]
					case i < len(list):
						column = columnOfShape(shape, nameOf(list[i], 0).Fold())
					}
					if r, ok := v.(stmt.Real); ok && column != nil && kindOf(column.Type) == stmt.TypeInt && float64(r) == math.Trunc(float64(r)) {
						n++
						break
					}
				}
			}
			return n
		}
		q, err := NewSequence(rand.New(rand.NewPCG(15, 16)))
		is.NotError(t, err)
		// A table with a column of INT or INTEGER, which every seed of the shape's random numbers does
		// not give.
		var table stmt.Name
		for i := 0; ; i++ {
			table = stmt.Name{Raw: fmt.Sprint("t", i), Value: fmt.Sprint("t", i)}
			if slices.ContainsFunc(q.shapeOf(table), func(c stmt.Column) bool { return kindOf(c.Type) == stmt.TypeInt }) {
				break
			}
		}
		without := wholeReals(q, table)
		q.Mistyped()
		with := wholeReals(q, table)
		t.Log(without, with)
		is.True(t, without < 150, "few whole reals of a value of any type")
		is.True(t, with > without+150, "one insert in eight more, of those that give the column an integer")
	})
}

func TestSequence_createIndex(t *testing.T) {
	t.Run("should give columns of the shape, each once, and now and then one that it does not have", func(t *testing.T) {
		q, err := NewSequence(rand.New(rand.NewPCG(11, 12)))
		is.NotError(t, err)
		table := stmt.Name{Raw: "t", Value: "t"}
		shape := q.shapeOf(table)
		missing, descending := 0, 0
		for range 2000 {
			columns := q.createIndex(stmt.Name{Raw: "i", Value: "i"}, table)
			seen := map[string]bool{}
			for _, c := range columns {
				fold := nameOf(c.Name.Raw, 0).Fold()
				is.True(t, !seen[fold], "a column twice")
				seen[fold] = true
				if columnOfShape(shape, fold) == nil {
					missing++
				}
				if c.Descending {
					descending++
				}
			}
			is.True(t, len(columns) > 0 && len(columns) <= len(shape)+2)
		}
		is.True(t, missing > 100 && missing < 300, fmt.Sprint("one create index in twelve with a column that the shape does not have: ", missing))
		is.True(t, descending > 400, fmt.Sprint("a column in three descending: ", descending))
		is.Equal(t, storedIndexes, len(q.indexed["t"]))
	})

	t.Run("should pin leading columns of a create index of the table in a WHERE, and more of them with Indexed", func(t *testing.T) {
		count := func(indexed bool) int {
			q, err := NewSequence(rand.New(rand.NewPCG(13, 14)))
			is.NotError(t, err)
			if indexed {
				q.Indexed()
			}
			table := stmt.Name{Raw: "t", Value: "t"}
			is.True(t, q.indexConditions(table) == nil, "no WHERE of an index before a create index")
			for range 20 {
				q.insert(table)
			}
			q.createIndex(stmt.Name{Raw: "i", Value: "i"}, table)
			pins := 0
			for range 2000 {
				for _, c := range q.where(table) {
					if c.Operator == stmt.OpEqual || c.Operator == stmt.OpIsNull {
						pins++
					}
				}
			}
			return pins
		}
		plain, indexed := count(false), count(true)
		is.True(t, plain > 0 && indexed > plain, fmt.Sprint(plain, " ", indexed))
	})
}

// typeOf the value v, which is not NULL: INT for an integer.
func typeOf(v stmt.Value) stmt.Type {
	switch v.(type) {
	case stmt.Real:
		return stmt.TypeReal
	case stmt.Text:
		return stmt.TypeText
	case stmt.Blob:
		return stmt.TypeBlob
	}
	return stmt.TypeInt
}

// valueKey of a value, for a comparison of two in a test.
func valueKey(v stmt.Value) string {
	text, _ := Literal(rand.New(rand.NewPCG(0, 0)), v)
	return text
}
