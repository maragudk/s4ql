package stmt

import (
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

// createTable: CREATE TABLE name (column INT PRIMARY KEY) and the options STRICT and WITHOUT ROWID,
// in either order, separated by a comma.
func (r *recognizer) createTable(c *ast.CreateTable) *CreateTable {
	r.accept(c)
	r.take(c.Create)
	r.take(c.Table)
	table := r.tableName(c.Name)
	r.take(c.LParen)
	column := r.column(c.Columns[0])
	if len(c.Commas) > 0 && r.isNext(c.Commas[0]) {
		// The comma after the column: a second column or a table constraint follows it.
		if len(c.Columns) > 1 {
			r.reject("second column")
		}
		r.reject("table constraint")
	}
	r.take(c.RParen)
	r.tableOptions(c)
	return &CreateTable{Table: table, Column: column}
}

// column: name INT PRIMARY KEY.
func (r *recognizer) column(d *ast.ColumnDef) Name {
	r.accept(d)
	name := r.name(d.Name)
	if d.Type == nil {
		r.reject("column without a type")
	}
	if !isWord(d.Type.Names[0], "int") {
		r.reject("column type other than INT")
	}
	r.take(d.Type.Names[0])
	if len(d.Type.Names) > 1 || d.Type.LParen.Present() {
		r.reject("column type other than INT")
	}
	if len(d.Constraints) == 0 {
		r.reject("column without PRIMARY KEY")
	}
	pk, ok := d.Constraints[0].(*ast.PrimaryKeyConstraint)
	if !ok {
		r.unexpected()
	}
	r.accept(pk)
	r.take(pk.Primary)
	r.take(pk.Key)
	return name
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

// insert: INSERT INTO table (column) VALUES (value).
func (r *recognizer) insert(c *ast.Insert) *Insert {
	r.accept(c)
	if r.isNext(c.Insert) && c.Insert.Kind == token.Replace {
		r.reject("REPLACE")
	}
	r.take(c.Insert)
	r.take(c.Into)
	r.accept(c.Table)
	table := r.tableName(c.Table.Name)
	// Without a column list, VALUES, SELECT, or DEFAULT follows the table.
	if c.Select != nil && r.at() == c.Select.Bounds().Start || r.isNext(c.Default) {
		r.reject("INSERT without a column list")
	}
	r.take(c.LParen)
	column := r.name(c.Columns.Names[0])
	if len(c.Columns.Names) > 1 {
		r.reject("second column")
	}
	r.take(c.RParen)
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
	value, span := r.integer(row.Exprs[0])
	r.take(row.RParen)
	return &Insert{Table: table, Column: column, Value: value, ValueSpan: span}
}

// integer value: a decimal integer literal that fits in 64 bits, with at most one - before it.
func (r *recognizer) integer(e ast.Expr) (int64, Span) {
	start := r.at()
	negative := false
	if u, ok := e.(*ast.Unary); ok {
		if u.Op.Kind != token.Minus {
			r.reject(unaryName(u.Op))
		}
		r.accept(u)
		r.take(u.Op)
		if v, ok := u.X.(*ast.Unary); ok {
			if v.Op.Kind == token.Minus || v.Op.Kind == token.Plus {
				r.reject("second sign")
			}
			r.reject(unaryName(v.Op))
		}
		negative = true
		e = u.X
	}
	l, ok := e.(*ast.Literal)
	if !ok {
		r.unexpected()
	}
	if what := literalName(l.Value); what != "" {
		r.reject(what)
	}
	digits := l.Value.Text
	if negative {
		digits = "-" + digits
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		r.reject("integer outside 64 bits")
	}
	r.take(l.Value)
	return v, Span{Start: start, End: l.Value.End()}
}

// literalName of the value of a literal that is not a decimal integer, or "" for a decimal integer.
func literalName(t ast.Token) string {
	switch t.Kind {
	case token.Integer:
		if strings.HasPrefix(t.Text, "0x") || strings.HasPrefix(t.Text, "0X") {
			return "hex literal"
		}
		return ""
	case token.Float:
		return "real number"
	case token.QNumber:
		return "number with a digit separator"
	case token.String:
		return "string"
	case token.Blob:
		return "blob"
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

// selectCmd: SELECT * FROM table.
func (r *recognizer) selectCmd(c *ast.SelectCmd) *Select {
	r.accept(c)
	r.accept(c.Select)
	core, ok := c.Select.First.(*ast.SimpleSelect)
	if !ok {
		r.unexpected()
	}
	r.accept(core)
	r.take(core.Select)
	rc := core.Columns[0]
	if (rc.Expr != nil || rc.Table.Present()) && r.at() == rc.Bounds().Start {
		r.reject("result column other than *")
	}
	r.take(rc.Star)
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
	return &Select{Table: r.tableName(source.Name)}
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
// the storage core does not hold a table or a column without a name.
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
