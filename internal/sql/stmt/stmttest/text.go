package stmttest

import (
	"slices"
	"strings"
	"unicode/utf8"

	"maragu.dev/s4ql/internal/sql/stmt"
)

// The text of the piece of work text functions that a [Sequence] writes, ||, lower, and hex, by the
// hooks that [Sequence.textHooks] wraps, all of it drawn from the stream of the area of the text
// functions.

// hasText reports whether the statement s has text of ||, lower, or hex: in the SET of an update, or
// on a side of a condition of its WHERE or of the WHERE of a subquery.
func hasText(s stmt.Statement) bool {
	var es []stmt.Expression
	where, subs := whereOf(s)
	if u, ok := s.(*stmt.Update); ok {
		for _, a := range u.Set {
			es = append(es, a.Value.Expression)
		}
	}
	for _, q := range subs {
		where = append(where[:len(where):len(where)], q.Where...)
	}
	for _, c := range where {
		es = append(es, c.Expression, c.Value.Expression)
	}
	return slices.ContainsFunc(es, isText)
}

// isText reports whether e is text: a concatenation or a call at its root.
func isText(e stmt.Expression) bool {
	return e.Kind == stmt.ExprConcat || e.Kind == stmt.ExprCall
}

// concatNode and callNode are the nodes of text of the trees that the hooks of a sequence give the
// generator.
func concatNode(x, y stmt.Expression) stmt.Expression {
	return operatorNode(stmt.ExprConcat, x, y)
}

func callNode(f stmt.Function, x stmt.Expression) stmt.Expression {
	return stmt.Expression{Kind: stmt.ExprCall, Function: f, Operands: []stmt.Expression{x}}
}

func lowerNode(x stmt.Expression) stmt.Expression {
	return callNode(stmt.FuncLower, x)
}

func hexNode(x stmt.Expression) stmt.Expression {
	return callNode(stmt.FuncHex, x)
}

// textPieces are the small texts that the hooks of text add to a value: empty, ASCII in both cases,
// with a quote, and letters that are not ASCII, which lower leaves as they are.
var textPieces = []string{"", "a", "B", "m_", "x'y", "É", "à", "ÀBc"}

// textHooks wrap the hooks of the WHERE of a select, of an update, and of a delete, so that their
// conditions and assignments, and the WHERE of their subqueries, have text, as [Sequence.textWhere]
// and [Sequence.textSet] draw it, one select in 3*[Profile.TextFunctions] the WHERE of
// [Sequence.textKeyWhere], and one in 2*[Profile.TextFunctions] of the others that of
// [Sequence.textEdgeWhere]. The assignments are drawn first, then the conditions of the statement,
// and then those of each subquery in the order of the text.
func (q *Sequence) textHooks() {
	where, update, del := q.g.Where, q.g.Update, q.g.Delete
	q.g.Where = func(table stmt.Name) []stmt.Condition {
		cs := where(table)
		switch r := q.rTextFunctions; {
		case r.IntN(3*q.profile.TextFunctions) == 0:
			cs = q.textKeyWhere(table, cs)
		case r.IntN(2*q.profile.TextFunctions) == 0:
			cs = q.textEdgeWhere(table, cs)
		}
		cs = q.textWhere(table, cs)
		q.textInner()
		return cs
	}
	q.g.Update = func(table stmt.Name) ([]stmt.Assignment, []stmt.Condition) {
		set, cs := update(table)
		set, cs = q.textSet(table, set), q.textWhere(table, cs)
		q.textInner()
		return set, cs
	}
	q.g.Delete = func(table stmt.Name) []stmt.Condition {
		cs := q.textWhere(table, del(table))
		q.textInner()
		return cs
	}
}

// textInner gives the WHERE of each subquery that the hooks of the statement gave text, as
// [Sequence.textWhere] draws it for the table of the subquery.
func (q *Sequence) textInner() {
	for i := range q.inner {
		q.inner[i].Where = q.textWhere(nameOf(q.inner[i].Table.Raw, 0), q.inner[i].Where)
	}
}

// textKeyWhere of a select of table, in place of the conditions cs: the key, where it is of type
// TEXT, equal to a text without a column whose value is one that an insert gave the key, as
// [Sequence.textConstantOf] draws it, which gets the row of the key, or one time in three between
// two such texts, which reads a range of the key, or one time in six equal to a text that is NULL,
// which reads no row; or cs where the key is of another type or the sequence keeps no value of it.
func (q *Sequence) textKeyWhere(table stmt.Name, cs []stmt.Condition) []stmt.Condition {
	r := q.rTextFunctions
	shape := q.shapeOf(table)
	key := shape[slices.IndexFunc(shape, func(c stmt.Column) bool { return c.PrimaryKey })]
	stored := q.textsOf(table.Fold(), key.Name.Value)
	if classOf(&key, nil) != classText || len(stored) == 0 {
		return cs
	}
	at := func(op stmt.Operator) stmt.Condition {
		return stmt.Condition{Column: stmt.Name{Raw: q.spell(r, key.Name.Value)}, Operator: op, Value: stmt.Term{Expression: q.textConstantOf(r, stored[r.IntN(len(stored))])}}
	}
	if r.IntN(6) == 0 {
		// NULL, which makes the access read no row.
		return []stmt.Condition{{Column: stmt.Name{Raw: q.spell(r, key.Name.Value)}, Operator: stmt.OpEqual, Value: stmt.Term{Expression: q.textConstantOf(r, stmt.Null{})}}}
	}
	if r.IntN(3) == 0 {
		return []stmt.Condition{at([]stmt.Operator{stmt.OpGreater, stmt.OpGreaterOrEqual}[r.IntN(2)]), at([]stmt.Operator{stmt.OpLess, stmt.OpLessOrEqual}[r.IntN(2)])}
	}
	return []stmt.Condition{at(stmt.OpEqual)}
}

// textEdgeWhere of a select of table, in place of the conditions cs: one comparison of a TEXT column
// of the shape, of the column with a text without a column at the edge of the length limit, as
// [Sequence.textEdgeConstant] draws it, or one time in two of a text of the column at the edge for a
// value that an insert gave it, as [Sequence.textEdge] draws it, with a piece of text, so that the
// outcome of the select is that of the edge; or cs where the shape has no TEXT column.
func (q *Sequence) textEdgeWhere(table stmt.Name, cs []stmt.Condition) []stmt.Condition {
	r := q.rTextFunctions
	texts := q.columnsOf(q.shapeOf(table), classText)
	if len(texts) == 0 {
		return cs
	}
	column := texts[r.IntN(len(texts))]
	raw := q.spell(r, column.Name.Value)
	op := comparisons[r.IntN(len(comparisons))]
	if r.IntN(2) == 0 {
		return []stmt.Condition{{Column: stmt.Name{Raw: raw}, Operator: op, Value: stmt.Term{Expression: q.textEdgeConstant(r)}}}
	}
	e := q.textEdge(r, columnNode(raw), q.textsOf(table.Fold(), column.Name.Value))
	return []stmt.Condition{{Expression: e, Operator: op, Value: stmt.Term{Value: stmt.Text(textPieces[r.IntN(len(textPieces))])}}}
}

// textsOf the column of the fold of the table of the fold: the texts that the sequence keeps for it,
// without NULL.
func (q *Sequence) textsOf(table, column string) []stmt.Text {
	var texts []stmt.Text
	for _, v := range q.stored[[2]string{table, column}] {
		if t, ok := v.(stmt.Text); ok {
			texts = append(texts, t)
		}
	}
	return texts
}

// textWhere gives the conditions cs of a WHERE of table, of a select, an update, a delete, or a
// subquery, with one comparison in [Profile.TextFunctions] that is not a test for NULL and has no
// expression made one of text, as [Sequence.textCondition] draws it. One comparison in half of
// [Profile.TextFunctions] whose value is a subquery tests in place of its column a text of it, as
// [Sequence.textTestedOf] draws it, which the subquery is then compared with; of a column of another
// type than TEXT only as a fault of the area, a text of a TEXT column of the shape, which s4ql
// refuses beside a subquery of another type. The others are as they were.
func (q *Sequence) textWhere(table stmt.Name, cs []stmt.Condition) []stmt.Condition {
	r, p := q.rTextFunctions, q.profile
	if len(cs) == 0 {
		return cs
	}
	shape := q.shapeOf(table)
	cs = slices.Clone(cs)
	for i, c := range cs {
		if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull || c.Expression.Present() || c.Value.Expression.Present() {
			continue
		}
		if c.Value.Subquery > 0 {
			if r.IntN(max(1, p.TextFunctions/2)) != 0 {
				continue
			}
			column := columnOfShape(shape, nameOf(c.Column.Raw, 0).Fold())
			raw := c.Column.Raw
			if classOf(column, nil) != classText {
				texts := q.columnsOf(shape, classText)
				if len(texts) == 0 || !q.fault(r, areaTextFunctions, 4) {
					continue
				}
				raw = q.spell(r, texts[r.IntN(len(texts))].Name.Value)
			}
			cs[i].Column, cs[i].Expression = stmt.Name{}, q.textTestedOf(r, raw)
			continue
		}
		if r.IntN(p.TextFunctions) == 0 {
			cs[i] = q.textCondition(r, table, shape, c)
		}
	}
	return cs
}

// columnsOf the shape of the class.
func (q *Sequence) columnsOf(shape []stmt.Column, class string) []stmt.Column {
	var of []stmt.Column
	for _, c := range shape {
		if classOf(&c, nil) == class {
			of = append(of, c)
		}
	}
	return of
}

// textTestedOf the column of the raw text raw, as the side that a comparison tests: lower of it, it
// and a piece of text, a piece and it, or lower of it and a piece, each of the type text.
func (q *Sequence) textTestedOf(r Rand, raw string) stmt.Expression {
	piece := valueNode(stmt.Text(textPieces[r.IntN(len(textPieces))]))
	switch r.IntN(4) {
	case 0:
		return lowerNode(columnNode(raw))
	case 1:
		return concatNode(columnNode(raw), piece)
	case 2:
		return concatNode(piece, columnNode(raw))
	}
	return lowerNode(concatNode(columnNode(raw), piece))
}

// textCondition gives the comparison c of table, of a column of the shape or of a name that the shape
// does not have, with text, drawn from r. A comparison of a name that the shape does not have tests lower of
// it, whose name s4ql looks up as SQLite does. One of a column of a number stays as it is, but as a
// fault of the area, a text compared with the column or a column of a number in a text, which
// s4ql refuses; and one of a BLOB column compares hex of the column with hex of the value or with a
// text, or, as a fault, puts the column in lower or in a concatenation, or compares it with a text.
// One of a TEXT column, in 20 draws:
//
//   - four times, the value is a text without a column whose value is that of c, as
//     [Sequence.textConstantOf] draws it, so that the condition serves the key or an index as c
//     would;
//   - two times, the comparison tests a text of the column against the same text of the value;
//   - once, the value is a text of another TEXT column, or the column alone;
//   - once, the comparison tests a text of the column against a plain value, as
//     [Sequence.textPlainOf] draws it;
//   - once, both sides are texts of columns;
//   - four times, the comparison tests a text of the column and an argument whose length takes the
//     text of a value that an insert gave the column to the length limit, one byte short of it, or one
//     byte over it, as [Sequence.textEdge] draws it;
//   - three times, the value is a text without a column at the length limit, one byte short of it, or one
//     byte over it, as [Sequence.textEdgeConstant] draws it;
//   - once, the comparison tests a text of the column with NULL in it;
//   - once, the comparison tests a nested text, lower of hex of the column and a piece, or the value
//     is goqite's id, 'm_' || lower(hex(?)) of a blob of 16 bytes;
//   - and two times, as a fault of the area one time in two, an argument of another type than its
//     place takes, a number or a blob, or a column of a number or a blob in a text; and else a value
//     as for the first case.
func (q *Sequence) textCondition(r Rand, table stmt.Name, shape []stmt.Column, c stmt.Condition) stmt.Condition {
	column := columnOfShape(shape, nameOf(c.Column.Raw, 0).Fold())
	raw := c.Column.Raw
	v := c.Value.Value
	if column == nil {
		c.Column, c.Expression = stmt.Name{}, lowerNode(columnNode(raw))
		return c
	}
	switch classOf(column, nil) {
	case classInteger, classReal:
		if !q.fault(r, areaTextFunctions, 3) {
			return c
		}
		if texts := q.columnsOf(shape, classText); len(texts) > 0 && r.IntN(2) == 0 {
			// A text compared with a column of a number.
			c.Value, c.ValueFirst = stmt.Term{Expression: lowerNode(columnNode(q.spell(r, texts[r.IntN(len(texts))].Name.Value)))}, false
			return c
		}
		c.Column, c.Expression = stmt.Name{}, []stmt.Expression{hexNode(columnNode(raw)), concatNode(columnNode(raw), valueNode(stmt.Text("a")))}[r.IntN(2)]
		c.Value = stmt.Term{Value: stmt.Text("31")}
		return c
	case classBlob:
		switch n := r.IntN(6); {
		case n < 2:
			c.Column, c.Expression = stmt.Name{}, hexNode(columnNode(raw))
			c.Value = stmt.Term{Value: stmt.Text([]string{"", "00", "61", "A"}[r.IntN(4)])}
		case n < 3 && q.fault(r, areaTextFunctions, 2):
			c.Column, c.Expression = stmt.Name{}, []stmt.Expression{lowerNode(columnNode(raw)), concatNode(valueNode(stmt.Text("a")), columnNode(raw))}[r.IntN(2)]
			c.Value = stmt.Term{Value: stmt.Text("a")}
		case n < 4 && q.fault(r, areaTextFunctions, 2):
			// A text compared with a BLOB column.
			c.Value, c.ValueFirst = stmt.Term{Expression: hexNode(valueNode(stmt.Blob{0x61}))}, false
		default:
			c.Column, c.Expression = stmt.Name{}, hexNode(columnNode(raw))
			c.Value = stmt.Term{Expression: hexNode(valueNode(orNull(v)))}
		}
		return c
	}
	text, _ := v.(stmt.Text)
	tested := func(e stmt.Expression, value stmt.Term) stmt.Condition {
		c.Column, c.Expression, c.Value = stmt.Name{}, e, value
		return c
	}
	switch n := r.IntN(20); {
	case n < 4:
		c.Value = stmt.Term{Expression: q.textConstantOf(r, v)}
	case n < 6:
		piece := valueNode(stmt.Text(textPieces[r.IntN(len(textPieces))]))
		x, y := columnNode(raw), valueNode(v)
		if v == stmt.Value(stmt.Null{}) || v == nil {
			y = valueNode(stmt.Null{})
		}
		switch r.IntN(4) {
		case 0:
			return tested(lowerNode(x), stmt.Term{Expression: lowerNode(y)})
		case 1:
			return tested(concatNode(x, piece), stmt.Term{Expression: concatNode(y, piece)})
		case 2:
			return tested(concatNode(piece, x), stmt.Term{Expression: concatNode(piece, y)})
		}
		return tested(hexNode(x), stmt.Term{Expression: hexNode(y)})
	case n < 7:
		other := q.otherText(r, shape, raw)
		c.ValueFirst = false
		if r.IntN(3) == 0 {
			c.Value = stmt.Term{Expression: columnNode(other)}
			return c
		}
		c.Value = stmt.Term{Expression: q.textTestedOf(r, other)}
	case n < 8:
		return tested(q.textTestedOf(r, raw), stmt.Term{Value: q.textPlainOf(r, v)})
	case n < 9:
		c.ValueFirst = false
		return tested(q.textTestedOf(r, raw), stmt.Term{Expression: q.textTestedOf(r, q.otherText(r, shape, raw))})
	case n < 13:
		return tested(q.textEdge(r, columnNode(raw), q.textsOf(table.Fold(), column.Name.Value)), stmt.Term{Value: stmt.Text(textPieces[r.IntN(len(textPieces))])})
	case n < 16:
		c.Value = stmt.Term{Expression: q.textEdgeConstant(r)}
	case n < 17:
		null := valueNode(stmt.Null{})
		switch r.IntN(3) {
		case 0:
			return tested(concatNode(columnNode(raw), null), stmt.Term{Value: orNull(v)})
		case 1:
			return tested(concatNode(hexNode(null), columnNode(raw)), stmt.Term{Expression: concatNode(valueNode(stmt.Text("")), valueNode(text))})
		}
		c.Value = stmt.Term{Expression: lowerNode(null)}
	case n < 18:
		if r.IntN(2) == 0 {
			return tested(lowerNode(hexNode(concatNode(columnNode(raw), valueNode(stmt.Text(textPieces[r.IntN(len(textPieces))]))))), stmt.Term{Value: stmt.Text("6d5f")})
		}
		c.Value = stmt.Term{Expression: q.goqiteID(r)}
	default:
		if !q.fault(r, areaTextFunctions, 2) {
			c.Value = stmt.Term{Expression: q.textConstantOf(r, v)}
			return c
		}
		return tested(q.textFault(r, shape, raw), stmt.Term{Value: stmt.Text("a")})
	}
	return c
}

// otherText of the shape at random: the raw text of a spelling of the name of a TEXT column other
// than the one of raw, or raw where the shape has none.
func (q *Sequence) otherText(r Rand, shape []stmt.Column, raw string) string {
	fold := nameOf(raw, 0).Fold()
	others := slices.DeleteFunc(q.columnsOf(shape, classText), func(c stmt.Column) bool { return c.Name.Value == fold })
	if len(others) == 0 {
		return raw
	}
	return q.spell(r, others[r.IntN(len(others))].Name.Value)
}

// textFault of the TEXT column of the raw text raw of the shape, drawn from r: a text that s4ql
// refuses for the type of an operand, with the column: an argument that is a number or a blob in a
// concatenation or in lower, or a number in hex, each as often, beside the column; or, one time in
// two where the shape has one, a column of the shape of a number or a blob in a concatenation, in
// lower, or in hex.
func (q *Sequence) textFault(r Rand, shape []stmt.Column, raw string) stmt.Expression {
	var others []stmt.Column
	for _, c := range shape {
		if class := classOf(&c, nil); class != classText {
			others = append(others, c)
		}
	}
	if len(others) > 0 && r.IntN(2) == 0 {
		x := columnNode(q.spell(r, others[r.IntN(len(others))].Name.Value))
		switch r.IntN(3) {
		case 0:
			return lowerNode(x)
		case 1:
			return concatNode(columnNode(raw), x)
		}
		return hexNode(x)
	}
	number := valueNode([]stmt.Value{stmt.Integer(1), stmt.Real(1.5)}[r.IntN(2)])
	blob := valueNode(stmt.Blob{0x61})
	return []stmt.Expression{concatNode(columnNode(raw), number), concatNode(blob, columnNode(raw)), concatNode(lowerNode(number), columnNode(raw)),
		concatNode(lowerNode(blob), columnNode(raw)), concatNode(columnNode(raw), hexNode(number))}[r.IntN(5)]
}

// textPlainOf the value v of a comparison of a TEXT column, for a side that it tests in a text: v, a
// piece of text, or, as a fault of the area one time in three, a value of another type, a number or a
// blob, which s4ql refuses against text, or NULL, which no text equals.
func (q *Sequence) textPlainOf(r Rand, v stmt.Value) stmt.Value {
	if !q.fault(r, areaTextFunctions, 3) {
		if r.IntN(2) == 0 && v != nil {
			return v
		}
		return stmt.Text(textPieces[r.IntN(len(textPieces))])
	}
	return []stmt.Value{stmt.Integer(1), stmt.Real(0.5), stmt.Blob{0x01}, stmt.Null{}}[r.IntN(4)]
}

// textConstantOf the value v, for a TEXT column: a text without a column whose value is v, where v is
// text: v split in two at a character, the empty text before or after it, lower of v with letters
// in upper case where v has none, or lower of v where it has, which is another text then, each one
// time in three in another concatenation with the empty text; and for NULL, or a value of another
// type, which the generator writes as a ?, NULL or the value in a concatenation or in lower, which
// is NULL for NULL and refused for the others.
func (q *Sequence) textConstantOf(r Rand, v stmt.Value) stmt.Expression {
	t, ok := v.(stmt.Text)
	if !ok {
		if r.IntN(2) == 0 {
			return lowerNode(valueNode(orNull(v)))
		}
		return concatNode(valueNode(orNull(v)), valueNode(stmt.Text(textPieces[r.IntN(len(textPieces))])))
	}
	var e stmt.Expression
	switch r.IntN(4) {
	case 0:
		var cuts []int
		for i := range string(t) {
			cuts = append(cuts, i)
		}
		cut := len(t)
		if len(cuts) > 0 {
			cut = cuts[r.IntN(len(cuts))]
		}
		e = concatNode(valueNode(t[:cut]), valueNode(t[cut:]))
	case 1:
		e = concatNode(valueNode(stmt.Text("")), valueNode(t))
	case 2:
		e = concatNode(valueNode(t), valueNode(stmt.Text("")))
	default:
		if asciiLower(string(t)) == string(t) {
			e = lowerNode(valueNode(stmt.Text(randomCase(r, string(t)))))
		} else {
			e = lowerNode(valueNode(t))
		}
	}
	if r.IntN(3) == 0 {
		e = concatNode(e, valueNode(stmt.Text("")))
	}
	return e
}

// orNull is v, or NULL for nil.
func orNull(v stmt.Value) stmt.Value {
	if v == nil {
		return stmt.Null{}
	}
	return v
}

// asciiLower of s: s with the ASCII letters A to Z in lower case, as SQLite's lower gives it.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}

// The edges of the length limit that [Sequence.textEdge] and [Sequence.textEdgeConstant] draw: the longest
// text of each that SQLite gives under the limit [LengthLimit], one byte shorter, and one byte
// longer, which fails with `string or blob too big`. A concatenation fails for a result longer than
// the limit, lower for an argument as long as the limit, and hex where twice the length of its
// argument and one is longer than the limit.
var edgeDeltas = []int{-1, 0, 1}

// edgeLength of the argument of a text of the function f at the edge delta of the limit: the length
// of the operands of a concatenation for f 0, of the argument of lower, or of that of hex.
func edgeLength(f stmt.Function, delta int) int {
	switch f {
	case stmt.FuncLower:
		return LengthLimit - 1 + delta
	case stmt.FuncHex:
		return (LengthLimit-1)/2 + delta
	}
	return LengthLimit + delta
}

// textEdge of the column x of a comparison, whose column holds the texts, drawn from r: x and an argument
// in a concatenation, in lower, or in hex, whose length takes the text of x of one of the texts,
// where it has one, to the edge of the length limit, one byte short of it, or one byte over it, as
// [edgeLength] gives them; and x alone in lower or hex where the texts are none or too long.
func (q *Sequence) textEdge(r Rand, x stmt.Expression, texts []stmt.Text) stmt.Expression {
	f := []stmt.Function{0, stmt.FuncLower, stmt.FuncHex}[r.IntN(3)]
	if len(texts) == 0 {
		return hexNode(x)
	}
	n := edgeLength(f, edgeDeltas[r.IntN(len(edgeDeltas))]) - len(texts[r.IntN(len(texts))])
	if n < 0 {
		return lowerNode(x)
	}
	e := concatNode(x, valueNode(stmt.Text(strings.Repeat("e", min(n, LengthLimit)))))
	if f != 0 {
		e = callNode(f, e)
	}
	return e
}

// textEdgeConstant is a text without a column at the edge of the length limit, one byte short of it, or
// one byte over it, as [edgeLength] gives them, drawn from r: two arguments in a concatenation, lower
// of an argument, or hex of a blob or of a text.
func (q *Sequence) textEdgeConstant(r Rand) stmt.Expression {
	delta := edgeDeltas[r.IntN(len(edgeDeltas))]
	switch r.IntN(3) {
	case 0:
		n := edgeLength(0, delta)
		left := 1 + r.IntN(n-1)
		return concatNode(valueNode(stmt.Text(strings.Repeat("c", left))), valueNode(stmt.Text(strings.Repeat("d", n-left))))
	case 1:
		return lowerNode(valueNode(stmt.Text(strings.Repeat("L", edgeLength(stmt.FuncLower, delta)))))
	}
	n := edgeLength(stmt.FuncHex, delta)
	if r.IntN(2) == 0 {
		return hexNode(valueNode(stmt.Blob(strings.Repeat("\x01", n))))
	}
	return hexNode(valueNode(stmt.Text(strings.Repeat("h", n))))
}

// goqiteID is goqite's id of a message, 'm_' || lower(hex(?)), with a blob of 16 bytes at random
// from r.
func (q *Sequence) goqiteID(r Rand) stmt.Expression {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(r.IntN(256))
	}
	return concatNode(valueNode(stmt.Text("m_")), lowerNode(hexNode(valueNode(stmt.Blob(b)))))
}

// textSet gives the assignments set of an update of table with one in [Profile.TextFunctions] given
// text, drawn from the stream of the area. An assignment of a column of a number or a blob stays as
// it is, but as a fault of the area, a text for it, which s4ql refuses. One of a TEXT column, in 12
// draws: four times the column and a piece of text, lower of it, or lower of it and a piece, which
// the update computes from its row; once a text without a column equal to its value, as
// [Sequence.textConstantOf] draws it; once goqite's id; once a text of another column, lower of a
// TEXT column, hex of a BLOB column, or another TEXT column and the column; once a text without a
// column at the edge of the length limit, as [Sequence.textEdgeConstant] draws it, or one time in two
// the column and an argument at the edge for a value that an insert gave it, as [Sequence.textEdge]
// draws it, so that the update fails on the row of the value; once NULL in a
// concatenation with the column, or hex of NULL, which is the empty text; once a nested text, lower of
// hex of another column and the column; and two times, as a fault of the area one time in two, a text
// that s4ql refuses for the type of an operand, as [Sequence.textFault] draws it, and else a text as
// for the first case.
func (q *Sequence) textSet(table stmt.Name, set []stmt.Assignment) []stmt.Assignment {
	r, p := q.rTextFunctions, q.profile
	shape := q.shapeOf(table)
	set = slices.Clone(set)
	for i, a := range set {
		if a.Value.Expression.Present() || r.IntN(p.TextFunctions) != 0 {
			continue
		}
		column := columnOfShape(shape, nameOf(a.Column.Raw, 0).Fold())
		if column == nil {
			continue
		}
		self := q.spell(r, column.Name.Value)
		piece := valueNode(stmt.Text(textPieces[r.IntN(len(textPieces))]))
		var e stmt.Expression
		switch classOf(column, nil) {
		case classText:
			switch n := r.IntN(12); {
			case n < 4:
				e = []stmt.Expression{concatNode(columnNode(self), piece), concatNode(piece, columnNode(self)), lowerNode(columnNode(self)), lowerNode(concatNode(columnNode(self), piece))}[r.IntN(4)]
			case n < 5:
				e = q.textConstantOf(r, a.Value.Value)
			case n < 6:
				e = q.goqiteID(r)
			case n < 7:
				switch blobs := q.columnsOf(shape, classBlob); {
				case len(blobs) > 0 && r.IntN(2) == 0:
					e = hexNode(columnNode(q.spell(r, blobs[r.IntN(len(blobs))].Name.Value)))
				case r.IntN(2) == 0:
					e = lowerNode(columnNode(q.otherText(r, shape, self)))
				default:
					e = concatNode(columnNode(q.otherText(r, shape, self)), columnNode(self))
				}
			case n < 8:
				e = q.textEdgeConstant(r)
				if r.IntN(2) == 0 {
					e = q.textEdge(r, columnNode(self), q.textsOf(table.Fold(), column.Name.Value))
				}
			case n < 9:
				e = []stmt.Expression{concatNode(valueNode(stmt.Null{}), columnNode(self)), hexNode(valueNode(stmt.Null{}))}[r.IntN(2)]
			case n < 10:
				e = concatNode(lowerNode(hexNode(columnNode(q.otherText(r, shape, self)))), columnNode(self))
			case q.fault(r, areaTextFunctions, 2):
				e = q.textFault(r, shape, self)
			default:
				e = concatNode(columnNode(self), piece)
			}
		default:
			if !q.fault(r, areaTextFunctions, 4) {
				continue
			}
			// A text for a column of a number or a blob.
			e = []stmt.Expression{lowerNode(valueNode(stmt.Text("a"))), hexNode(valueNode(stmt.Blob{0x01})), concatNode(piece, valueNode(stmt.Text("1")))}[r.IntN(3)]
		}
		set[i].Value = stmt.Term{Expression: e}
	}
	return set
}

// textLike a, an argument of a text that runs again, drawn from r: for a text of more than 40 bytes,
// which [Generator.textValue] writes as a ?, one byte shorter or longer, so that a text at the edge of
// the length limit crosses it, and otherwise another value of its Go type, as [Sequence.like] gives
// it.
func (q *Sequence) textLike(r Rand, a any) any {
	switch v := a.(type) {
	case string:
		if len(v) > 40 && utf8.ValidString(v) {
			if r.IntN(2) == 0 {
				return v[:len(v)-1]
			}
			return v + "e"
		}
	case []byte:
		if len(v) > 20 {
			if r.IntN(2) == 0 {
				return v[:len(v)-1]
			}
			return append(slices.Clone(v), 0x01)
		}
	}
	return q.like(r, a)
}

// TextRefused are texts that the statement pass refuses for their text, with %[1]v for the name of a
// table and %[2]v for that of a column, each the refusal of the text that its comment names.
var TextRefused = []string{
	"select * from %[1]v where %[2]v = %[2]v || 1",                    // number in a concatenation
	"update %[1]v set %[2]v = %[2]v || -%[2]v",                        // number in a concatenation
	"delete from %[1]v where %[2]v = %[2]v || null",                   // NULL in a concatenation
	"select * from %[1]v where %[2]v = x'01' || %[2]v",                // blob in a concatenation
	"update %[1]v set %[2]v = lower(1)",                               // number as an argument
	"update %[1]v set %[2]v = hex(null)",                              // NULL as an argument
	"select * from %[1]v where lower(x'41') = %[2]v",                  // blob as an argument
	"update %[1]v set %[2]v = upper(%[2]v)",                           // function other than lower or hex
	"select * from %[1]v where %[2]v = lower()",                       // wrong number of arguments
	"delete from %[1]v where %[2]v = hex(%[2]v, %[2]v)",               // wrong number of arguments
	"select * from %[1]v where %[2]v = hex(*)",                        // * as an argument
	"update %[1]v set %[2]v = lower(distinct %[2]v)",                  // DISTINCT or ALL
	"select * from %[1]v where %[2]v = lower(%[2]v) filter (where 1)", // FILTER clause
	"select * from %[1]v where [lower](%[2]v) = 'a'",                  // name in brackets
	"select * from %[1]v where %[2]v * lower(%[2]v) = 1",              // text in arithmetic
	"update %[1]v set %[2]v = (%[2]v || 'a') * 2",                     // text in arithmetic
	"select * from %[1]v where %[2]v = lower((%[2]v))",                // parentheses
	"update %[1]v set %[2]v = 'a' || (hex(%[2]v))",                    // parentheses
	"select lower(%[2]v) from %[1]v",                                  // result column other than a column or *
	"select * from %[1]v where lower(%[2]v) is null",                  // function call
	"insert into %[1]v values ('a' || 'b')",                           // binary operator
	"select * from %[1]v where 'a' || 'b' = lower('ab')",              // value compared with a value
	"select * from %[1]v where %[2]v || current_time = 'a'",           // current date or time
}
