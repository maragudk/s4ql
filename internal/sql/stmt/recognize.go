package stmt

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/token"
)

// recognizer of a supported statement in a syntax tree. It walks the tree in the order of the text
// and takes the tokens that the supported form expects, one after the other: each token it takes
// must be the next token of the statement. It builds the statement only from the tokens it took. A
// token that it does not take stops it, and so does a token in a field that it never reads, because
// that token is then the next one when the recognizer takes a later token, or is left over at the
// end.
//
// A check of a token's content, such as whether a name is reserved, runs when the token is the next
// one, so that a reject names the token at the offset it gives.
type recognizer struct {
	src  string
	tree *ast.Statement
	// tokens of the statement, every one that the parser read for it, in the order of the text, from
	// the tokenizer. By the ownership rule of the tree they are the tokens of [ast.AllTokens]; since
	// they come from the text, a token that no node holds, or that a node holds but the text does not
	// have, is never taken.
	tokens []ast.Token
	// next is the index in tokens of the next token to take.
	next int
	// accepted nodes, which the recognizer has read as part of a supported form, besides the root.
	// The name of a token that stops it comes from them; see [recognizer.describe].
	accepted map[ast.Node]bool
	// parameters that the recognizer has taken so far. SQLite numbers a plain ? one more than the one
	// before it in the text, and the recognizer takes the tokens in the order of the text, inside a
	// subquery too.
	parameters int
	// subqueries of the WHERE of the statement that the recognizer has read so far, in the order of
	// the text, and the height of the select of each, as [recognizer.selectOf] gives it.
	subqueries []Select
	heights    []int
	// inner is the closing parenthesis of the subquery whose select the recognizer reads, or the
	// absent token outside a subquery.
	inner ast.Token
}

// stop is the panic with which the recognizer unwinds when the statement is not supported.
type stop struct {
	err *NotSupportedError
}

// recognize the statement of tree, which the parser read from src, or say why it is not supported.
func recognize(src string, tree *ast.Statement) (s Statement, err *NotSupportedError) {
	r := &recognizer{src: src, tree: tree, tokens: parser.Tokens(src, tree), accepted: map[ast.Node]bool{}}
	defer func() {
		if v := recover(); v != nil {
			st, ok := v.(stop)
			if !ok {
				panic(v)
			}
			s, err = nil, st.err
		}
	}()
	s = r.statement()
	// The check that holds for any kind of statement and any node: a token that the recognizer did
	// not take makes the statement not supported. A command of a kind that the recognizer does not
	// know is rejected here too, since none of its tokens was taken.
	if r.next != len(r.tokens) {
		r.unexpected()
	}
	s.setSpan(Span{Start: r.tokens[0].Offset, End: r.tokens[len(r.tokens)-1].End()})
	return s, nil
}

// accept the node n as part of a supported form.
func (r *recognizer) accept(n ast.Node) {
	r.accepted[n] = true
}

// isNext reports whether t is the next token of the statement. An absent token never is.
func (r *recognizer) isNext(t ast.Token) bool {
	return r.next < len(r.tokens) && r.tokens[r.next] == t
}

// take the token t, which must be the next token of the statement; otherwise the statement is not
// supported, for the token that is next.
func (r *recognizer) take(t ast.Token) ast.Token {
	if !r.isNext(t) {
		r.unexpected()
	}
	r.next++
	return t
}

// at gives the offset of the next token of the statement, or of the end of the statement if every
// token is taken.
func (r *recognizer) at() int {
	if r.next < len(r.tokens) {
		return r.tokens[r.next].Offset
	}
	return r.tree.Bounds().End
}

// reject the statement as not supported, with what, at the next token.
func (r *recognizer) reject(what string) {
	panic(stop{&NotSupportedError{What: what, Offset: r.at()}})
}

// unexpected next token: the statement is not supported, for what that token is part of.
func (r *recognizer) unexpected() {
	if r.next == len(r.tokens) {
		r.reject("statement")
	}
	r.reject(r.describe(r.tokens[r.next]))
}

// statement of the tree, or nil for a command of another kind.
func (r *recognizer) statement() Statement {
	if r.tree.Query.Present() {
		// EXPLAIN QUERY PLAN, whose EXPLAIN is the first token of the statement. EXPLAIN alone is named
		// by its field.
		r.reject("EXPLAIN QUERY PLAN")
	}
	var s Statement
	switch c := r.tree.Cmd.(type) {
	case *ast.CreateTable:
		s = r.createTable(c)
	case *ast.Insert:
		s = r.insert(c)
	case *ast.SelectCmd:
		s = r.selectCmd(c)
	case *ast.Update:
		s = r.update(c)
	case *ast.Delete:
		s = r.delete(c)
	case *ast.Drop:
		s = r.drop(c)
	case *ast.CreateIndex:
		s = r.createIndex(c)
	}
	if r.tree.Semi.Present() {
		r.take(r.tree.Semi)
	}
	return s
}

// createTable: CREATE TABLE, an optional IF NOT EXISTS, name (columns) and the options STRICT and
// WITHOUT ROWID, in either order, separated by a comma. The columns are one or more, separated by
// commas, and exactly one is the primary key.
func (r *recognizer) createTable(c *ast.CreateTable) *CreateTable {
	r.accept(c)
	r.take(c.Create)
	r.take(c.Table)
	ifNotExists := r.ifExists(c.IfNotExists)
	if ifNotExists {
		r.notIf(c.Name)
	}
	table := r.tableName(c.Name)
	r.take(c.LParen)
	s := &CreateTable{IfNotExists: ifNotExists, Table: table}
	key := false
	for i, d := range c.Columns {
		if i > 0 {
			// The commas between the columns come first, before those of the table constraints.
			r.take(c.Commas[i-1])
		}
		column := r.column(d, key)
		key = key || column.PrimaryKey
		s.Columns = append(s.Columns, column)
	}
	// A table constraint follows a comma after the last column. Both rejects are for the token at
	// the end of the columns: one that the recognizer did not take before it is named by the take of
	// the parenthesis.
	if len(c.Constraints) > 0 && r.isNext(c.Commas[len(c.Columns)-1]) {
		r.reject("table constraint")
	}
	if !key && r.isNext(c.RParen) {
		r.reject("table without PRIMARY KEY")
	}
	r.take(c.RParen)
	r.tableOptions(c)
	// SQLite keeps the text up to its last token when the table has no options; with them, which
	// it always has here, up to the token that ends the statement: the semicolon, or the end of the
	// text.
	end := len(r.src)
	if r.tree.Semi.Present() {
		end = r.tree.Semi.Offset
	}
	s.SQL = "CREATE TABLE " + r.src[table.Offset:end]
	return s
}

// columnTypes are the types of a column, by their bare word in lower case.
var columnTypes = map[string]Type{"int": TypeInt, "integer": TypeInteger, "real": TypeReal, "text": TypeText, "blob": TypeBlob}

// otherType is the name of a reject for a column type that is not one of [columnTypes].
const otherType = "column type other than INT, INTEGER, REAL, TEXT, or BLOB"

// column: name type and its constraints, where the type is one bare word of [columnTypes], in any
// case, and the constraints are PRIMARY KEY, NOT NULL, and DEFAULT, each at most once, in any order,
// or none. key reports whether an earlier column of the table is its primary key, so that this one must
// not be.
func (r *recognizer) column(d *ast.ColumnDef, key bool) Column {
	r.accept(d)
	c := Column{Name: r.name(d.Name)}
	if d.Type == nil {
		r.reject("column without a type")
	}
	// A word in quotes has its quotes in its text, so it is none of the types.
	typ, ok := columnTypes[asciiLower(d.Type.Names[0].Text)]
	if !ok {
		r.reject(otherType)
	}
	r.take(d.Type.Names[0])
	if len(d.Type.Names) > 1 || d.Type.LParen.Present() {
		r.reject(otherType)
	}
	c.Type = typ
	for _, k := range d.Constraints {
		switch k := k.(type) {
		case *ast.PrimaryKeyConstraint:
			if (key || c.PrimaryKey) && r.isNext(k.Primary) {
				// SQLite gives `table "t" has more than one primary key` while it parses.
				r.reject("second PRIMARY KEY")
			}
			r.accept(k)
			r.take(k.Primary)
			r.take(k.Key)
			c.PrimaryKey = true
		case *ast.NullConstraint:
			if !k.Not.Present() && r.isNext(k.Null) {
				// SQLite reads a NULL constraint as none: the column takes NULL without it.
				r.reject("NULL constraint")
			}
			if c.NotNull || !k.Not.Present() {
				r.unexpected()
			}
			r.accept(k)
			r.take(k.Not)
			r.take(k.Null)
			c.NotNull = true
		case *ast.DefaultConstraint:
			if c.Default.Value != nil && r.isNext(k.Default) {
				// SQLite keeps the last DEFAULT of the column, and the text of the others.
				r.reject("second DEFAULT")
			}
			r.accept(k)
			r.take(k.Default)
			c.Default = r.defaultValue(k, c.Type)
		default:
			r.unexpected()
		}
	}
	return c
}

// defaultValue of the DEFAULT d of a column of the type typ, after the keyword DEFAULT: a literal, as
// a value of an insert is, of the type of the column or NULL. SQLite takes a literal of any type
// there, and converts it or refuses it when an insert stores it; by the exact types of an insert,
// s4ql could never store a literal of another type than its column, so the declaration is not
// supported. SQLite reads a name after DEFAULT, also one in quotes, as a string, and TRUE and FALSE
// as 1 and 0; a DEFAULT in parentheses is an expression.
func (r *recognizer) defaultValue(d *ast.DefaultConstraint, typ Type) Default {
	start := r.at()
	// A name is the token after DEFAULT, which the caller has taken, and an absent one has no text.
	if isWord(d.Name, "true") || isWord(d.Name, "false") {
		r.reject("TRUE or FALSE")
	}
	if d.LParen.Present() {
		// Before value reads the expression inside, which may be a literal that it refuses for itself.
		r.unexpected()
	}
	// Any other name is no literal, and value names it, the next token.
	negative := false
	if d.Sign.Present() {
		r.sign(d.Sign, d.X)
		negative = true
	}
	v, span := r.value(d.X, negative, start)
	if !takes(typ, v) {
		panic(stop{&NotSupportedError{What: "DEFAULT of another type than its column", Offset: start}})
	}
	return Default{Value: v, Span: span}
}

// takes reports whether a column of the type typ takes the value v by the exact types of an insert:
// an integer for INT and INTEGER, a real for REAL, a text for TEXT, a blob for BLOB, and NULL for
// every type.
func takes(typ Type, v Value) bool {
	switch v.(type) {
	case Integer:
		return typ == TypeInt || typ == TypeInteger
	case Real:
		return typ == TypeReal
	case Text:
		return typ == TypeText
	case Blob:
		return typ == TypeBlob
	}
	return true
}

// tableOptions: STRICT and WITHOUT ROWID, each once, in either order, with a comma between them.
func (r *recognizer) tableOptions(c *ast.CreateTable) {
	var strict, withoutRowid bool
	for i, o := range c.Options {
		if i == 2 {
			r.reject("table option")
		}
		if i == 1 {
			// A comma before the first option would have stopped the take of that option, so the
			// first comma is the one between the two.
			r.take(c.OptionCommas[0])
		}
		if o.Without.Present() {
			if withoutRowid {
				r.reject("table option")
			}
			r.take(o.Without)
			if !isWord(o.Name, "rowid") {
				r.reject("table option")
			}
			r.take(o.Name)
			withoutRowid = true
			continue
		}
		if r.isNext(o.Name) && (strict || !isWord(o.Name, "strict")) {
			r.reject("table option")
		}
		r.take(o.Name)
		strict = true
	}
	switch {
	case !strict && !withoutRowid:
		r.reject("table without STRICT and WITHOUT ROWID")
	case !strict:
		r.reject("table without STRICT")
	case !withoutRowid:
		r.reject("table without WITHOUT ROWID")
	}
}

// isWord reports whether t is the bare word w, in any case of its letters. A word in quotes has its
// quotes in its text, and no keyword is spelled as one of the words that slice 1 takes.
func isWord(t ast.Token, w string) bool {
	return asciiLower(t.Text) == w
}

// insert: INSERT INTO table, an optional list of columns, VALUES with one row of values, where each
// value is a literal or a parameter, and an optional RETURNING.
func (r *recognizer) insert(c *ast.Insert) *Insert {
	r.accept(c)
	if r.isNext(c.Insert) && c.Insert.Kind == token.Replace {
		r.reject("REPLACE")
	}
	r.take(c.Insert)
	r.take(c.Into)
	r.accept(c.Table)
	s := &Insert{Table: r.tableName(c.Table.Name)}
	if c.Columns != nil {
		r.take(c.LParen)
		r.accept(c.Columns)
		seen := map[string]bool{}
		for i, t := range c.Columns.Names {
			if i > 0 {
				r.take(c.Columns.Commas[i-1])
			}
			if r.isNext(t) && quotingName(t) == "" && seen[asciiLower(dequote(t))] {
				// SQLite takes the first value for the column and leaves the others unused.
				r.reject("column named twice")
			}
			n := r.name(t)
			seen[n.Fold()] = true
			s.Columns = append(s.Columns, n)
		}
		r.take(c.RParen)
	}
	if c.Select == nil {
		r.unexpected()
	}
	r.accept(c.Select)
	values, ok := c.Select.First.(*ast.Values)
	if !ok {
		if r.at() == c.Select.First.Bounds().Start {
			r.reject("INSERT with SELECT")
		}
		r.unexpected()
	}
	r.accept(values)
	r.take(values.Values)
	row := values.Rows[0]
	r.accept(row)
	r.take(row.LParen)
	for i, e := range row.Exprs {
		if i > 0 {
			r.take(row.Commas[i-1])
		}
		s.Values = append(s.Values, r.term(e))
	}
	r.take(row.RParen)
	s.Returning = r.returning(c.Returning)
	return s
}

// returning: an optional RETURNING and its columns, the last clause of an insert, an update, or a
// delete, read as a select list is read: * alone, or one or more bare names of columns, separated by
// commas, which may name a column more than once. SQLite takes any result column there, also an
// expression, an alias, and a column with the name of its table; the pass supports what it supports
// in a select list. A clause that the parser read before a token that the recognizer has not taken,
// such as the ON of an upsert, stops at that token.
func (r *recognizer) returning(ret *ast.Returning) Returning {
	if ret == nil {
		return Returning{}
	}
	r.accept(ret)
	start := r.take(ret.Returning).Offset
	var columns []Name
	for i, rc := range ret.Columns {
		if i > 0 {
			r.take(ret.Commas[i-1])
		}
		columns = r.resultColumn(rc, len(ret.Columns), columns)
	}
	return Returning{Span: Span{Start: start, End: r.tokens[r.next-1].End()}, Columns: columns}
}

// term e: a parameter, or a literal.
func (r *recognizer) term(e ast.Expr) Term {
	var v Term
	if p, ok := e.(*ast.Variable); ok {
		v.Parameter, v.Span = r.parameter(p)
	} else {
		v.Value, v.Span = r.literal(e)
	}
	return v
}

// maxParameters of a statement in SQLite, SQLITE_MAX_VARIABLE_NUMBER, which the binding in
// internal/sqlite keeps at its default.
const maxParameters = 32766

// parameter v: a plain ?, after the parameters that the recognizer has taken. SQLite numbers a ? one
// more than the largest number before it, and each parameter before it is a plain ? too, so its
// number is one more than theirs. After [maxParameters] parameters, it is `parameter beyond SQLite's
// limit`.
func (r *recognizer) parameter(v *ast.Variable) (int, Span) {
	if r.isNext(v.Name) && v.Name.Text != "?" {
		if v.Name.Text[0] == '?' {
			r.reject("numbered parameter")
		}
		r.reject("named parameter")
	}
	if r.parameters == maxParameters && r.isNext(v.Name) {
		// SQLite gives `too many SQL variables` while it parses, whatever the database holds.
		r.reject("parameter beyond SQLite's limit")
	}
	r.accept(v)
	r.take(v.Name)
	r.parameters++
	return r.parameters, Span{Start: v.Name.Offset, End: v.Name.End()}
}

// literal value: a decimal integer literal that fits in 64 bits or a real literal, with at most one
// - before either; a string; a blob; or NULL.
func (r *recognizer) literal(e ast.Expr) (Value, Span) {
	start := r.at()
	negative := false
	if u, ok := e.(*ast.Unary); ok {
		r.accept(u)
		r.sign(u.Op, u.X)
		negative = true
		e = u.X
	}
	return r.value(e, negative, start)
}

// sign op before the expression x of a literal: a -, which it takes. SQLite reads NULL after a sign
// as NULL, and the pass supports neither that nor any other sign.
func (r *recognizer) sign(op ast.Token, x ast.Expr) {
	if op.Kind == token.Plus && isNull(x) {
		r.reject("NULL after a sign")
	}
	if op.Kind != token.Minus {
		r.reject(unaryName(op))
	}
	r.take(op)
	switch x := x.(type) {
	case *ast.Unary:
		if x.Op.Kind == token.Minus || x.Op.Kind == token.Plus {
			r.reject("second sign")
		}
		r.reject(unaryName(x.Op))
	case *ast.Variable:
		r.reject("parameter after a sign")
	}
	if isNull(x) {
		r.reject("NULL after a sign")
	}
}

// value of the literal e, after a - if negative, which starts at start: as [recognizer.literal]
// gives it.
func (r *recognizer) value(e ast.Expr, negative bool, start int) (Value, Span) {
	if c, ok := e.(*ast.ColumnRef); ok && r.isNext(c.Column) && !c.Table.Present() &&
		(isWord(c.Column, "true") || isWord(c.Column, "false")) {
		// SQLite reads a bare TRUE or FALSE where no column of that name is, as in VALUES, as 1 or 0.
		r.reject("TRUE or FALSE")
	}
	l, ok := e.(*ast.Literal)
	if !ok {
		r.unexpected()
	}
	t := l.Value
	var v Value
	switch t.Kind {
	case token.Integer:
		if strings.HasPrefix(t.Text, "0x") || strings.HasPrefix(t.Text, "0X") {
			r.reject("hex literal")
		}
		digits := t.Text
		if negative {
			digits = "-" + digits
		}
		i, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			r.reject("integer outside 64 bits")
		}
		v = Integer(i)
	case token.Float:
		// SQLite reads the literal after the sign, and negates the result.
		f := atof(t.Text)
		if math.IsInf(f, 0) {
			r.reject("infinite real number")
		}
		if negative {
			f = -f
		}
		v = Real(f)
	case token.String:
		if negative {
			r.reject("string after a sign")
		}
		v = Text(strings.ReplaceAll(t.Text[1:len(t.Text)-1], "''", "'"))
	case token.Blob:
		if negative {
			r.reject("blob after a sign")
		}
		b, err := hex.DecodeString(t.Text[2 : len(t.Text)-1])
		if err != nil {
			// The tokenizer gives a blob only for an even number of hex digits.
			panic(fmt.Sprintf("stmt: the blob literal %q: %v", t.Text, err))
		}
		v = Blob(b)
	case token.Null:
		v = Null{}
	default:
		r.reject(literalName(t))
	}
	r.take(t)
	return v, Span{Start: start, End: t.End()}
}

// isNull reports whether e is the literal NULL.
func isNull(e ast.Expr) bool {
	l, ok := e.(*ast.Literal)
	return ok && l.Value.Kind == token.Null
}

// literalName of the value of a literal that is not supported.
func literalName(t ast.Token) string {
	switch t.Kind {
	case token.QNumber:
		return "number with a digit separator"
	case token.CTimeKw:
		return "current date or time"
	default:
		return "literal"
	}
}

// unaryName of a unary operator other than -.
func unaryName(op ast.Token) string {
	switch op.Kind {
	case token.Plus:
		return "+ sign"
	case token.BitNot:
		return "~ operator"
	case token.Not:
		return "NOT operator"
	default:
		return "unary operator"
	}
}

// selectCmd: SELECT, * or the names of one or more columns, FROM table, and an optional WHERE, ORDER
// BY, and LIMIT, in that order.
func (r *recognizer) selectCmd(c *ast.SelectCmd) *Select {
	r.accept(c)
	s, _ := r.selectOf(c.Select)
	return s
}

// selectOf sel: a select as [recognizer.selectCmd] reads it, or the select of a subquery, which has
// one result column. It gives the select with its span from its SELECT to the end of its last
// token, and its height as SQLite counts it for a subquery: that of its deepest expression, a WHERE,
// a LIMIT, which is 2, or a column, which is 1.
func (r *recognizer) selectOf(sel *ast.Select) (*Select, int) {
	r.accept(sel)
	core, ok := sel.First.(*ast.SimpleSelect)
	if !ok {
		r.unexpected()
	}
	r.accept(core)
	start := r.take(core.Select).Offset
	var columns []Name
	for i, rc := range core.Columns {
		if i > 0 {
			r.take(core.Commas[i-1])
		}
		if i == 1 && r.inner.Present() && r.at() == rc.Bounds().Start {
			// SQLite gives `row value misused` when it prepares the statement, once it has found the
			// tables.
			r.reject("subquery of more than one column")
		}
		columns = r.resultColumn(rc, len(core.Columns), columns)
	}
	if core.From == nil {
		if r.next == len(r.tokens) || r.isNext(r.tree.Semi) || r.isNext(r.inner) {
			r.reject("SELECT without FROM")
		}
		r.unexpected()
	}
	r.accept(core.From)
	r.take(core.From.From)
	item := core.From.Items[0]
	r.accept(item)
	source, ok := item.Source.(*ast.TableSource)
	if !ok {
		r.unexpected()
	}
	r.accept(source)
	s := &Select{Table: r.tableName(source.Name), Columns: columns}
	height := 1
	if core.Where != nil {
		if r.inner.Present() {
			s.Where, height = r.where(core.Where)
		} else {
			s.Where, s.Subqueries = r.statementWhere(core.Where)
		}
	}
	if core.OrderBy != nil {
		s.OrderBy = r.orderBy(core.OrderBy)
	}
	if core.Limit != nil {
		// SQLite's LIMIT is an expression of its own, one deeper than its count.
		s.Limit = r.limit(core.Limit)
		height = max(height, 2)
	}
	s.Span = Span{Start: start, End: r.tokens[r.next-1].End()}
	return s, height
}

// subquery sq, the value of a comparison: SELECT and its clauses in parentheses, as
// [recognizer.selectOf] reads them, in the WHERE of a statement and not in that of a subquery. It
// adds the select to the subqueries of the statement, and gives the term that names it, and its
// height as SQLite counts it, one more than that of its select.
func (r *recognizer) subquery(sq *ast.Subquery) (Term, int) {
	if r.inner.Present() && r.isNext(sq.LParen) {
		r.reject("subquery in a subquery")
	}
	r.accept(sq)
	start := r.take(sq.LParen).Offset
	r.inner = sq.RParen
	s, height := r.selectOf(sq.Select)
	r.inner = ast.Token{}
	if height+1 > maxDepth && r.isNext(sq.RParen) {
		// SQLite gives `Expression tree is too large (maximum depth 1000)` while it parses.
		r.reject("WHERE deeper than SQLite's limit")
	}
	r.take(sq.RParen)
	r.subqueries = append(r.subqueries, *s)
	r.heights = append(r.heights, height)
	return Term{Subquery: len(r.subqueries), Span: Span{Start: start, End: sq.RParen.End()}}, height + 1
}

// orderBy: ORDER BY and one or more columns, separated by commas, of which no two are one name.
func (r *recognizer) orderBy(o *ast.OrderBy) []OrderColumn {
	r.accept(o)
	r.take(o.Order)
	r.take(o.By)
	seen := map[string]bool{}
	var columns []OrderColumn
	for i, term := range o.Terms {
		if i > 0 {
			r.take(o.Commas[i-1])
		}
		c := r.orderColumn(term, seen)
		seen[c.Name.Fold()] = true
		columns = append(columns, c)
	}
	return columns
}

// orderColumn o of an ORDER BY, after the columns of seen, by their folds: the bare name of a column,
// or one in double quotes, that seen does not have, and ASC, DESC, or neither. SQLite reads any
// expression there, an integer literal as the position of a result column; the pass supports none.
// A column that an earlier one names is not supported: SQLite orders by the first, so the later one
// orders nothing.
func (r *recognizer) orderColumn(o *ast.OrderingTerm, seen map[string]bool) OrderColumn {
	r.accept(o)
	switch x := o.X.(type) {
	case *ast.ColumnRef:
		switch {
		case x.Table.Present():
			r.reject("column with the name of a table")
		case isTrueOrFalse(x):
			// SQLite reads a bare TRUE or FALSE as 1 or 0 where the table has no column of that name.
			r.reject("TRUE or FALSE")
		case r.isNext(x.Column) && quotingName(x.Column) == "" && seen[asciiLower(dequote(x.Column))]:
			r.reject("column named twice")
		}
		r.accept(x)
	case *ast.Collate:
		r.reject("COLLATE")
	case *ast.Paren:
		r.reject("parentheses")
	default:
		r.reject("ordering term other than a column")
	}
	c := OrderColumn{Name: r.name(o.X.(*ast.ColumnRef).Column)}
	if o.Order.Present() {
		r.take(o.Order)
		c.Descending = o.Order.Kind == token.Desc
	}
	return c
}

// limit: LIMIT and the number of rows: a decimal integer
// literal without a sign that fits in 64 bits, or a plain ?. SQLite takes any expression there,
// which it converts to an integer when the select runs: a real or a text that holds an integer is
// that integer, and any other value is `datatype mismatch`; a negative number is no limit. By the
// exact types of s4ql, only an integer that is a limit is supported. LIMIT with OFFSET, also as
// LIMIT and two numbers, is not supported.
func (r *recognizer) limit(l *ast.Limit) Term {
	r.accept(l)
	r.take(l.Limit)
	v := r.limitCount(l.X)
	if l.Sep.Present() {
		// The next token is OFFSET, or the comma of `LIMIT a, b`, whose first number is the offset and
		// the second the count.
		r.reject("OFFSET")
	}
	return v
}

// limitCount: the number of rows of a LIMIT, the expression x, which starts at the next token.
func (r *recognizer) limitCount(x ast.Expr) Term {
	switch x := x.(type) {
	case *ast.Variable:
		var v Term
		v.Parameter, v.Span = r.parameter(x)
		return v
	case *ast.Literal:
		t := x.Value
		switch t.Kind {
		case token.Integer:
			if strings.HasPrefix(t.Text, "0x") || strings.HasPrefix(t.Text, "0X") {
				r.reject("hex literal")
			}
			i, err := strconv.ParseInt(t.Text, 10, 64)
			if err != nil {
				// SQLite reads the literal as a real, which is no integer.
				r.reject("integer outside 64 bits")
			}
			r.take(t)
			return Term{Value: Integer(i), Span: Span{Start: t.Offset, End: t.End()}}
		case token.Float, token.String, token.Blob, token.Null:
			r.reject("LIMIT other than an integer")
		}
		r.reject(literalName(t))
	case *ast.Unary:
		switch x.Op.Kind {
		case token.Minus:
			// SQLite reads a negative number as no limit.
			r.reject("negative LIMIT")
		case token.Plus:
			r.reject("+ sign")
		}
	case *ast.ColumnRef:
		if isTrueOrFalse(x) {
			// SQLite reads a bare TRUE or FALSE as 1 or 0.
			r.reject("TRUE or FALSE")
		}
	case *ast.Paren:
		r.reject("parentheses")
	}
	r.reject("LIMIT of an expression")
	return Term{}
}

// statementWhere w of a select, an update, or a delete, and not of a subquery: WHERE and its
// conditions, and its subqueries. When SQLite resolves the names of the WHERE, once it has found the
// tables, it counts the height of the WHERE and that of the select of each subquery in it together,
// and refuses more than [maxDepth] with `Expression tree is too large (maximum depth 1000)`. The
// pass refuses it at the first subquery that makes it too deep, once it has read the WHERE.
func (r *recognizer) statementWhere(w *ast.Where) ([]Condition, []Select) {
	where, height := r.where(w)
	for _, c := range where {
		if k := c.Value.Subquery; k > 0 && height+r.heights[k-1] > maxDepth {
			panic(stop{&NotSupportedError{What: "WHERE deeper than SQLite's limit", Offset: c.Value.Span.Start}})
		}
	}
	return where, r.subqueries
}

// where: WHERE and its conditions, and their height as SQLite counts it.
func (r *recognizer) where(w *ast.Where) ([]Condition, int) {
	r.accept(w)
	r.take(w.Where)
	return r.conditions(w.X)
}

// update: UPDATE table SET and one or more assignments, separated by commas, each a column = a value,
// where the value is a literal or a parameter, an optional WHERE, and an optional RETURNING.
func (r *recognizer) update(c *ast.Update) *Update {
	r.accept(c)
	r.take(c.Update)
	r.accept(c.Table)
	s := &Update{Table: r.tableName(c.Table.Name)}
	r.take(c.Set)
	seen := map[string]bool{}
	for i, a := range c.Assignments {
		if i > 0 {
			r.take(c.Commas[i-1])
		}
		if i == maxColumns && r.at() == a.Bounds().Start {
			// SQLite gives `too many columns in set list` while it parses, whatever the database holds.
			r.reject("SET beyond SQLite's limit")
		}
		r.accept(a)
		if !a.Column.Present() {
			// The assignment is to a list of columns in parentheses.
			r.unexpected()
		}
		if r.isNext(a.Column) && quotingName(a.Column) == "" && seen[asciiLower(dequote(a.Column))] {
			// SQLite gives the column the value of its last assignment, and leaves the others unused.
			r.reject("column named twice")
		}
		n := r.name(a.Column)
		seen[n.Fold()] = true
		r.take(a.Eq)
		s.Set = append(s.Set, Assignment{Column: n, Value: r.term(a.X)})
	}
	if c.Where != nil {
		s.Where, s.Subqueries = r.statementWhere(c.Where)
	}
	s.Returning = r.returning(c.Returning)
	return s
}

// delete: DELETE FROM table, an optional WHERE, and an optional RETURNING.
func (r *recognizer) delete(c *ast.Delete) *Delete {
	r.accept(c)
	r.take(c.Delete)
	r.take(c.From)
	r.accept(c.Table)
	s := &Delete{Table: r.tableName(c.Table.Name)}
	if c.Where != nil {
		s.Where, s.Subqueries = r.statementWhere(c.Where)
	}
	s.Returning = r.returning(c.Returning)
	return s
}

// conditions of a WHERE clause, the expression e: one condition, or conditions joined by AND, in the
// order of the text, and their height as SQLite counts it.
func (r *recognizer) conditions(e ast.Expr) ([]Condition, int) {
	// OR binds less tightly than AND, so the conditions before the first OR are the leftmost operand of
	// the ORs; that OR comes after them. The ORs are accepted, as the ANDs are below, so that a token
	// of those conditions that stops the recognizer is named for what holds it.
	or := false
	for isBinary(e, token.Or) {
		or = true
		r.accept(e)
		e = e.(*ast.Binary).X
	}
	// AND groups to the left, so `a AND b AND c` is `(a AND b) AND c`: the ANDs are a spine down the
	// left, whose last holds the first condition. They are all accepted before the first condition is
	// read, so that a token that stops the recognizer there is named for what holds it.
	var ands []*ast.Binary
	for isBinary(e, token.And) {
		b := e.(*ast.Binary)
		r.accept(b)
		ands = append(ands, b)
		e = b.X
	}
	c, height := r.condition(e)
	conditions := []Condition{c}
	for i := len(ands) - 1; i >= 0; i-- {
		// SQLite gives an AND the depth of its deeper operand and one more, and refuses an expression
		// deeper than maxDepth while it parses, whatever the database holds. A condition without a
		// subquery is at most 3 deep, so the depth of the AND is height+1 when that can be too much,
		// and the error is at the AND. A condition with a subquery can be deeper, and the AND is
		// checked again after it.
		if height+1 > maxDepth {
			r.reject("WHERE deeper than SQLite's limit")
		}
		r.take(ands[i].Op)
		c, h := r.condition(ands[i].Y)
		conditions = append(conditions, c)
		height = max(height, h) + 1
		if height > maxDepth {
			// The condition after the AND has a subquery that makes the AND too deep, which SQLite
			// refuses once it has read the condition, at the token after it.
			r.reject("WHERE deeper than SQLite's limit")
		}
	}
	if or {
		r.reject("OR")
	}
	return conditions, height
}

// maxDepth of an expression in SQLite, SQLITE_MAX_EXPR_DEPTH, which the binding in internal/sqlite
// keeps at its default.
const maxDepth = 1000

// depth of the condition e, which the recognizer took, as SQLite counts it: a name, a literal, and a
// parameter are 1 deep, and an operator is one deeper than its deepest operand, a sign too, on
// either side of the operator.
func depth(e ast.Expr) int {
	if b, ok := e.(*ast.Binary); ok {
		_, left := b.X.(*ast.Unary)
		_, right := b.Y.(*ast.Unary)
		if left || right {
			return 3
		}
	}
	return 2
}

// isBinary reports whether e is the binary operator of the kind k.
func isBinary(e ast.Expr, k token.Kind) bool {
	b, ok := e.(*ast.Binary)
	return ok && b.Op.Kind == k
}

// comparisons are the operators of a comparison, by the kind of their token.
var comparisons = map[token.Kind]Operator{
	token.EQ: OpEqual, token.NE: OpNotEqual, token.LT: OpLess, token.LE: OpLessOrEqual, token.GT: OpGreater,
	token.GE: OpGreaterOrEqual,
}

// condition e of a WHERE clause: a column compared with a literal, a parameter, or a scalar subquery,
// with one of the operators of [comparisons], on either side of the operator, or a column IS NULL or
// IS NOT NULL. It gives the condition and its height as SQLite counts it.
//
// A comparison with the value on the left, as `5 < a`, is read as the column compared with the value
// by the operator swapped, `a > 5`, with [Condition.ValueFirst] set; see [Condition] for why. The
// value is read as a value on the right is, with the same names for what is not supported, and its
// ? has its number in the order of the text. A value on either side is read as
// [recognizer.conditionValue] reads it.
//
// The next token is the first of e when it is called, since e follows WHERE or AND, and the first
// token of the left operand of a condition is the first of the condition.
func (r *recognizer) condition(e ast.Expr) (Condition, int) {
	switch e := e.(type) {
	case *ast.Is:
		return r.isCondition(e), depth(e)
	case *ast.Unary:
		if e.Op.Kind == token.Not {
			r.reject("NOT operator")
		}
	case *ast.ColumnRef:
		if isTrueOrFalse(e) {
			// SQLite reads a bare TRUE or FALSE as 1 or 0 where the table has no column of that name.
			r.reject("TRUE or FALSE")
		}
	}
	b, _ := e.(*ast.Binary)
	var op Operator
	if b != nil {
		op = comparisons[b.Op.Kind]
	}
	if op == 0 {
		r.unexpected()
	}
	r.accept(b)
	var c Condition
	var height int
	if isValue(b.X) {
		c = Condition{Operator: op.Swapped(), ValueFirst: true}
		c.Value, height = r.conditionValue(b.X)
		r.take(b.Op)
		c.Column = r.conditionColumn(b.Y, "value compared with a value")
	} else {
		c = Condition{Column: r.conditionColumn(b.X, ""), Operator: op}
		r.take(b.Op)
		if ref, ok := b.Y.(*ast.ColumnRef); ok && !isTrueOrFalse(ref) {
			r.reject("column compared with a column")
		}
		c.Value, height = r.conditionValue(b.Y)
	}
	if c.Value.Subquery == 0 {
		return c, depth(e)
	}
	// The column on the other side is 1 deep, and the subquery deeper.
	if height+1 > maxDepth {
		// SQLite refuses the comparison once it has read it, at the token after it.
		r.reject("WHERE deeper than SQLite's limit")
	}
	return c, height + 1
}

// conditionValue e of a comparison: a scalar subquery, as [recognizer.subquery] reads it, with its
// height, or a literal or a parameter, as [recognizer.term] reads it, with the height 0, which the
// caller does not use. A subquery after a - is not supported, at the subquery; one after a + is `+
// sign`, as for a parameter.
func (r *recognizer) conditionValue(e ast.Expr) (Term, int) {
	switch x := e.(type) {
	case *ast.Subquery:
		return r.subquery(x)
	case *ast.Unary:
		if _, ok := x.X.(*ast.Subquery); ok && x.Op.Kind == token.Minus && r.isNext(x.Op) {
			r.accept(x)
			r.take(x.Op)
			r.reject("subquery after a sign")
		}
	}
	return r.term(e), 0
}

// isCondition: a column IS NULL or IS NOT NULL.
func (r *recognizer) isCondition(e *ast.Is) Condition {
	r.accept(e)
	c := Condition{Column: r.conditionColumn(e.X, "value before IS"), Operator: OpIsNull}
	r.take(e.Is)
	if e.Not.Present() {
		r.take(e.Not)
		c.Operator = OpIsNotNull
	}
	if e.Distinct.Present() {
		r.reject("IS DISTINCT FROM")
	}
	if !isNull(e.Y) {
		switch e.Y.(type) {
		case *ast.Literal, *ast.Variable, *ast.ColumnRef, *ast.Unary, *ast.Subquery:
			r.reject("IS with a value other than NULL")
		}
		r.unexpected()
	}
	r.take(e.Y.(*ast.Literal).Value)
	return c
}

// conditionColumn of a condition, the expression x on either side of its operator, which starts at the next
// token: the bare name of a column. A literal or a parameter there, with a sign or without one, is
// not supported as value, the name of that form. Where the caller reads a value on the left as the
// value of the condition, it calls this only for a left side that is no value, with value "".
func (r *recognizer) conditionColumn(x ast.Expr, value string) Name {
	ref, ok := x.(*ast.ColumnRef)
	switch {
	case ok && ref.Table.Present():
		r.reject("column with the name of a table")
	case ok && isTrueOrFalse(ref):
		// SQLite reads a bare TRUE or FALSE as 1 or 0 where the table has no column of that name.
		r.reject("TRUE or FALSE")
	case isValue(x):
		r.reject(value)
	case !ok:
		r.unexpected()
	}
	r.accept(ref)
	return r.name(ref.Column)
}

// isValue reports whether x is a literal, a parameter, or a subquery, with a sign or without one.
func isValue(x ast.Expr) bool {
	if u, ok := x.(*ast.Unary); ok && (u.Op.Kind == token.Minus || u.Op.Kind == token.Plus) {
		x = u.X
	}
	switch x.(type) {
	case *ast.Literal, *ast.Variable, *ast.Subquery:
		return true
	}
	return false
}

// isTrueOrFalse reports whether c is the bare word TRUE or FALSE, without a table.
func isTrueOrFalse(c *ast.ColumnRef) bool {
	return !c.Table.Present() && (isWord(c.Column, "true") || isWord(c.Column, "false"))
}

// resultColumn rc of a select list or a RETURNING of n result columns, after the names of the columns
// before it: a * that is the only result column, or the bare name of a column. It gives the names with
// that of rc added.
func (r *recognizer) resultColumn(rc *ast.ResultColumn, n int, before []Name) []Name {
	if rc.Star.Present() && !rc.Table.Present() {
		if n > 1 && r.isNext(rc.Star) {
			r.reject("* and another result column")
		}
		r.take(rc.Star)
		return before
	}
	ref, ok := rc.Expr.(*ast.ColumnRef)
	if !ok || ref.Table.Present() {
		if r.at() == rc.Bounds().Start {
			r.reject("result column other than a column or *")
		}
		r.unexpected()
	}
	if r.isNext(ref.Column) && (isWord(ref.Column, "true") || isWord(ref.Column, "false")) {
		// SQLite reads a bare TRUE or FALSE as 1 or 0 where the table has no column of that name.
		r.reject("TRUE or FALSE")
	}
	r.accept(rc)
	r.accept(ref)
	return append(before, r.name(ref.Column))
}

// drop: DROP TABLE table, or DROP INDEX index, each with an optional IF EXISTS.
func (r *recognizer) drop(c *ast.Drop) Statement {
	r.accept(c)
	r.take(c.Drop)
	switch c.Object.Kind {
	case token.Table:
		r.take(c.Object)
		ifExists := r.ifExists(c.IfExists)
		return &DropTable{IfExists: ifExists, Table: r.tableName(c.Name)}
	case token.Index:
		r.take(c.Object)
		ifExists := r.ifExists(c.IfExists)
		return &DropIndex{IfExists: ifExists, Index: r.indexName(c.Name)}
	}
	r.reject("DROP " + strings.ToUpper(c.Object.Text))
	return nil
}

// createIndex: CREATE INDEX, an optional IF NOT EXISTS, index ON table and, in parentheses, one or
// more columns of the index, separated by commas.
func (r *recognizer) createIndex(c *ast.CreateIndex) *CreateIndex {
	r.accept(c)
	r.take(c.Create)
	r.take(c.Index)
	ifNotExists := r.ifExists(c.IfNotExists)
	if ifNotExists {
		r.notIf(c.Name)
	}
	s := &CreateIndex{IfNotExists: ifNotExists, Index: r.indexName(c.Name)}
	r.take(c.On)
	// The grammar takes one name after ON, without a schema.
	s.Table = r.tableName(ast.QualifiedName{Name: c.Table})
	r.take(c.LParen)
	seen := map[string]bool{}
	for i, o := range c.Columns {
		if i > 0 {
			r.take(c.Commas[i-1])
		}
		if i == maxColumns {
			// SQLite gives `too many columns in index` once it has found the table, before it looks for
			// the columns.
			r.reject("index beyond SQLite's limit")
		}
		column := r.indexColumn(o, seen)
		seen[column.Name.Fold()] = true
		s.Columns = append(s.Columns, column)
	}
	r.take(c.RParen)
	if c.Where != nil && r.isNext(c.Where.Where) {
		r.reject("partial index")
	}
	// SQLite keeps the text up to the token that ends the statement: the semicolon, or the end of the
	// text.
	end := len(r.src)
	if r.tree.Semi.Present() {
		end = r.tree.Semi.Offset
	}
	s.SQL = "CREATE INDEX " + r.src[s.Index.Offset:end]
	return s
}

// indexColumn o of a CREATE INDEX, after the columns of seen, by their folds: the bare name of a
// column, or one in double quotes, that seen does not have, and ASC, DESC, or neither. SQLite reads
// a string in single quotes there as a name too, and parentheses around a name as the name; the pass
// supports neither. Anything else is an expression, or a name with a COLLATE or with the name of its
// table, which SQLite refuses.
func (r *recognizer) indexColumn(o *ast.OrderingTerm, seen map[string]bool) IndexColumn {
	r.accept(o)
	switch x := o.X.(type) {
	case *ast.ColumnRef:
		switch {
		case x.Table.Present():
			// SQLite gives `the "." operator prohibited in index expressions`.
			r.reject("column with the name of a table")
		case isTrueOrFalse(x):
			// SQLite reads a bare TRUE or FALSE as 1 or 0 where the table has no column of that name.
			r.reject("TRUE or FALSE")
		case r.isNext(x.Column) && quotingName(x.Column) == "" && seen[asciiLower(dequote(x.Column))]:
			// SQLite makes an index of the column twice.
			r.reject("column named twice")
		}
		r.accept(x)
	case *ast.Literal:
		if x.Value.Kind == token.String {
			r.reject("single-quoted name")
		}
		r.reject("index on an expression")
	case *ast.Collate:
		r.reject("COLLATE")
	case *ast.Paren:
		r.reject("parentheses")
	default:
		r.reject("index on an expression")
	}
	c := IndexColumn{Name: r.name(o.X.(*ast.ColumnRef).Column)}
	if o.Order.Present() {
		r.take(o.Order)
		c.Descending = o.Order.Kind == token.Desc
	}
	return c
}

// ifExists takes IF NOT EXISTS or IF EXISTS, i, if the statement has it, and reports whether it has.
// The parser gives i only in the place of the clause, and only whole: IF and EXISTS, with NOT for
// IF NOT EXISTS, and without it for IF EXISTS.
func (r *recognizer) ifExists(i ast.IfNotExists) bool {
	if !i.If.Present() {
		return false
	}
	r.take(i.If)
	if i.Not.Present() {
		r.take(i.Not)
	}
	r.take(i.Exists)
	return true
}

// notIf rejects the name of a table or an index after IF NOT EXISTS, q, if it is the bare word IF,
// in any case of its letters. SQLite keeps the text of the statement from the name on in
// sqlite_schema, as CREATE TABLE IF ... or CREATE INDEX IF ..., which it cannot parse again: it
// gives `malformed database schema (if) - near "(": syntax error`, or near "on" for an index, and
// creates nothing. Without the clause, IF there starts the clause, so the name is a syntax error.
func (r *recognizer) notIf(q ast.QualifiedName) {
	if r.isNext(q.Name) && isWord(q.Name, "if") {
		r.reject("bare name IF after IF NOT EXISTS")
	}
}

// indexName: the name of an index, which must not be reserved, without a schema.
func (r *recognizer) indexName(q ast.QualifiedName) Name {
	if r.isNext(q.Name) && reservedPrefix(dequote(q.Name)) {
		// SQLite gives `object name reserved for internal use` for sqlite_ in CREATE INDEX, once it has
		// found the table, and `index associated with UNIQUE or PRIMARY KEY constraint cannot be
		// dropped` for the index of a primary key in DROP INDEX.
		r.reject("reserved index name")
	}
	return r.name(q.Name)
}

// tableName: the name of a table, which must not be reserved, without a schema.
func (r *recognizer) tableName(q ast.QualifiedName) Name {
	if r.isNext(q.Name) {
		// A name in brackets, backticks, or single quotes keeps its quotes in its value, so it is not
		// reserved, and name rejects it.
		if what := reservedName(dequote(q.Name)); what != "" {
			r.reject(what)
		}
	}
	return r.name(q.Name)
}

// name: a bare name, or one in double quotes, that is not empty. SQLite takes `""` for a name, and
// s4ql holds no table or column without a name.
func (r *recognizer) name(t ast.Token) Name {
	if what := quotingName(t); what != "" && r.isNext(t) {
		r.reject(what)
	}
	value := dequote(t)
	if value == "" && r.isNext(t) {
		r.reject("empty name")
	}
	r.take(t)
	return Name{Raw: t.Text, Value: value, Offset: t.Offset}
}

// quotingName gives why the quoting of the name t is not supported, or "" if it is: a single-quoted
// string, brackets, and backticks are not.
func quotingName(t ast.Token) string {
	switch {
	case t.Kind == token.String:
		return "single-quoted name"
	case t.Text[0] == '[':
		return "name in brackets"
	case t.Text[0] == '`':
		return "name in backticks"
	}
	return ""
}

// dequote the name t: the text of a bare name, and that of a name in double quotes without the quotes
// and with "" for each quote inside. Only a name of kind ID starts with a double quote.
func dequote(t ast.Token) string {
	if t.Text[0] == '"' {
		return strings.ReplaceAll(t.Text[1:len(t.Text)-1], `""`, `"`)
	}
	return t.Text
}
