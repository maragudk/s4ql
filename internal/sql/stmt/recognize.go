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
}

// stop is the panic with which the recognizer unwinds when the statement is not supported.
type stop struct {
	err *NotSupportedError
}

// recognize the statement of tree, which the parser read from src, or say why it is not supported.
func recognize(src string, tree *ast.Statement) (s Statement, err *NotSupportedError) {
	r := &recognizer{tree: tree, tokens: parser.Tokens(src, tree), accepted: map[ast.Node]bool{}}
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
	var s Statement
	switch c := r.tree.Cmd.(type) {
	case *ast.CreateTable:
		s = r.createTable(c)
	case *ast.Insert:
		s = r.insert(c)
	case *ast.SelectCmd:
		s = r.selectCmd(c)
	case *ast.Drop:
		s = r.drop(c)
	}
	if r.tree.Semi.Present() {
		r.take(r.tree.Semi)
	}
	return s
}

// createTable: CREATE TABLE name (columns) and the options STRICT and WITHOUT ROWID, in either
// order, separated by a comma. The columns are one or more, separated by commas, and exactly one is
// the primary key.
func (r *recognizer) createTable(c *ast.CreateTable) *CreateTable {
	r.accept(c)
	r.take(c.Create)
	r.take(c.Table)
	table := r.tableName(c.Name)
	r.take(c.LParen)
	s := &CreateTable{Table: table}
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
	return s
}

// columnTypes are the types of a column, by their bare word in lower case.
var columnTypes = map[string]Type{"int": TypeInt, "integer": TypeInteger, "real": TypeReal, "text": TypeText, "blob": TypeBlob}

// otherType is the name of a reject for a column type that is not one of [columnTypes].
const otherType = "column type other than INT, INTEGER, REAL, TEXT, or BLOB"

// column: name type and its constraints, where the type is one bare word of [columnTypes], in any
// case, and the constraints are PRIMARY KEY and NOT NULL, each at most once, in either order. The
// column has at least one of them. key reports whether an earlier column of the table is its primary
// key, so that this one must not be.
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
			if c.NotNull || !k.Not.Present() {
				r.unexpected()
			}
			r.accept(k)
			r.take(k.Not)
			r.take(k.Null)
			c.NotNull = true
		default:
			r.unexpected()
		}
	}
	switch {
	case c.PrimaryKey || c.NotNull:
	case key:
		r.reject("column without NOT NULL")
	default:
		r.reject("column without PRIMARY KEY or NOT NULL")
	}
	return c
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

// insert: INSERT INTO table, an optional list of columns, and VALUES with one row of values, where
// each value is a literal or a parameter.
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
			if r.isNext(t) && seen[asciiLower(dequote(t))] {
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
		var v Term
		if p, ok := e.(*ast.Variable); ok {
			v.Parameter, v.Span = r.parameter(p, s.Parameters())
		} else {
			v.Value, v.Span = r.literal(e)
		}
		s.Values = append(s.Values, v)
	}
	r.take(row.RParen)
	return s
}

// maxParameters of a statement in SQLite, SQLITE_MAX_VARIABLE_NUMBER, which the binding in
// internal/sqlite keeps at its default.
const maxParameters = 32766

// parameter v: a plain ?, after before parameters of the statement, which must be fewer than
// [maxParameters]. SQLite numbers a ? one more than
// the largest number before it, and each parameter before it is a plain ? too, so its number is one
// more than their number.
func (r *recognizer) parameter(v *ast.Variable, before int) (int, Span) {
	if r.isNext(v.Name) && v.Name.Text != "?" {
		if v.Name.Text[0] == '?' {
			r.reject("numbered parameter")
		}
		r.reject("named parameter")
	}
	if before == maxParameters && r.isNext(v.Name) {
		// SQLite gives `too many SQL variables` while it parses, whatever the database holds.
		r.reject("parameter beyond SQLite's limit")
	}
	r.accept(v)
	r.take(v.Name)
	return before + 1, Span{Start: v.Name.Offset, End: v.Name.End()}
}

// literal value: a decimal integer literal that fits in 64 bits or a real literal, with at most one
// - before either; a string; or a blob.
func (r *recognizer) literal(e ast.Expr) (Value, Span) {
	start := r.at()
	negative := false
	if u, ok := e.(*ast.Unary); ok {
		if u.Op.Kind != token.Minus {
			r.reject(unaryName(u.Op))
		}
		r.accept(u)
		r.take(u.Op)
		switch x := u.X.(type) {
		case *ast.Unary:
			if x.Op.Kind == token.Minus || x.Op.Kind == token.Plus {
				r.reject("second sign")
			}
			r.reject(unaryName(x.Op))
		case *ast.Variable:
			r.reject("parameter after a sign")
		}
		negative = true
		e = u.X
	}
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
	default:
		r.reject(literalName(t))
	}
	r.take(t)
	return v, Span{Start: start, End: t.End()}
}

// literalName of the value of a literal that is not supported.
func literalName(t ast.Token) string {
	switch t.Kind {
	case token.QNumber:
		return "number with a digit separator"
	case token.Null:
		return "NULL"
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

// selectCmd: SELECT, * or the names of one or more columns, FROM table.
func (r *recognizer) selectCmd(c *ast.SelectCmd) *Select {
	r.accept(c)
	r.accept(c.Select)
	core, ok := c.Select.First.(*ast.SimpleSelect)
	if !ok {
		r.unexpected()
	}
	r.accept(core)
	r.take(core.Select)
	var columns []Name
	for i, rc := range core.Columns {
		if i > 0 {
			r.take(core.Commas[i-1])
		}
		columns = r.resultColumn(rc, len(core.Columns), columns)
	}
	if core.From == nil {
		if r.next == len(r.tokens) || r.isNext(r.tree.Semi) {
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
	return &Select{Table: r.tableName(source.Name), Columns: columns}
}

// resultColumn rc of a select list of n result columns, after the names of the columns before it: a
// * that is the only result column, or the bare name of a column. It gives the names with that of rc
// added.
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

// drop: DROP TABLE table.
func (r *recognizer) drop(c *ast.Drop) *DropTable {
	r.accept(c)
	r.take(c.Drop)
	if c.Object.Kind != token.Table {
		r.reject("DROP " + strings.ToUpper(c.Object.Text))
	}
	r.take(c.Object)
	return &DropTable{Table: r.tableName(c.Name)}
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
