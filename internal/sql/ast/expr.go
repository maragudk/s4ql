package ast

// Expr is an expression: the nonterminals expr and term of parse.y.
type Expr interface {
	Node
	expr()
}

// exprBase of every expression node.
type exprBase struct{ base }

func (*exprBase) expr() {}

// Literal is a literal value, one token: NULL, an integer, a real, a number with digit separators,
// a string, a blob, or one of CURRENT_DATE, CURRENT_TIME, and CURRENT_TIMESTAMP. The kind of the
// token tells which.
//
// parse.y: term ::= NULL|FLOAT|BLOB. term ::= STRING. term ::= INTEGER. term ::= QNUMBER.
// term ::= CTIME_KW.
type Literal struct {
	exprBase
	Value Token
}

// Variable is a parameter: ?, ?NNN, :name, @name, #name, or $name.
//
// parse.y: expr ::= VARIABLE.
type Variable struct {
	exprBase
	Name Token
}

// ColumnRef is a name in an expression, with up to two qualifiers: column, table.column, or
// schema.table.column. Column is always present. A single name is an idj of parse.y (never a
// string), while a qualified name is made of nm, which can be a string.
//
// parse.y: expr ::= idj. expr ::= nm DOT nm. expr ::= nm DOT nm DOT nm.
type ColumnRef struct {
	exprBase
	Schema Token
	Dot1   Token // the dot after Schema
	Table  Token
	Dot2   Token // the dot after Table
	Column Token
}

// Paren is an expression in parentheses.
//
// parse.y: expr ::= LP expr RP.
type Paren struct {
	exprBase
	LParen Token
	X      Expr
	RParen Token
}

// Row is a row value: two or more expressions in parentheses.
//
// parse.y: expr ::= LP nexprlist COMMA expr RP.
type Row struct {
	exprBase
	LParen Token
	Exprs  []Expr
	Commas []Token
	RParen Token
}

// Subquery is a SELECT in parentheses, used as a value.
//
// parse.y: expr ::= LP select RP.
type Subquery struct {
	exprBase
	LParen Token
	Select *Select
	RParen Token
}

// Exists is EXISTS and a SELECT in parentheses. NOT EXISTS is a [Unary] NOT around it.
//
// parse.y: expr ::= EXISTS LP select RP.
type Exists struct {
	exprBase
	Exists Token
	LParen Token
	Select *Select
	RParen Token
}

// Unary is a prefix operator: NOT, ~, +, or -.
//
// parse.y: expr ::= NOT expr. expr ::= BITNOT expr. expr ::= PLUS|MINUS expr.
type Unary struct {
	exprBase
	Op Token
	X  Expr
}

// Binary is an infix operator with two operands: OR, AND, <, >, >=, <=, =, ==, <>, !=, &, |, <<, >>,
// +, -, *, /, %, ||, ->, or ->>.
//
// parse.y: expr ::= expr AND expr. expr ::= expr OR expr. expr ::= expr LT|GT|GE|LE expr.
// expr ::= expr EQ|NE expr. expr ::= expr BITAND|BITOR|LSHIFT|RSHIFT expr.
// expr ::= expr PLUS|MINUS expr. expr ::= expr STAR|SLASH|REM expr. expr ::= expr CONCAT expr.
// expr ::= expr PTR expr.
type Binary struct {
	exprBase
	X  Expr
	Op Token
	Y  Expr
}

// Is is X IS Y, X IS NOT Y, X IS NOT DISTINCT FROM Y, or X IS DISTINCT FROM Y. X IS NULL is this
// node with a [Literal] NULL as Y.
//
// parse.y: expr ::= expr IS expr. expr ::= expr IS NOT expr.
// expr ::= expr IS NOT DISTINCT FROM expr. expr ::= expr IS DISTINCT FROM expr.
type Is struct {
	exprBase
	X        Expr
	Is       Token
	Not      Token
	Distinct Token
	From     Token
	Y        Expr
}

// NullTest is X ISNULL, X NOTNULL, or X NOT NULL. For X ISNULL and X NOTNULL, Op is the one token
// and Null is absent; for X NOT NULL, Op is NOT and Null is NULL.
//
// parse.y: expr ::= expr ISNULL|NOTNULL. expr ::= expr NOT NULL.
type NullTest struct {
	exprBase
	X    Expr
	Op   Token
	Null Token
}

// Like is X [NOT] LIKE|GLOB|REGEXP|MATCH Y [ESCAPE Z].
//
// parse.y: likeop ::= LIKE_KW|MATCH. likeop ::= NOT LIKE_KW|MATCH. expr ::= expr likeop expr.
// expr ::= expr likeop expr ESCAPE expr.
type Like struct {
	exprBase
	X       Expr
	Not     Token
	Op      Token
	Y       Expr
	Escape  Token
	EscapeX Expr
}

// Between is X [NOT] BETWEEN Low AND High.
//
// parse.y: between_op ::= BETWEEN. between_op ::= NOT BETWEEN.
// expr ::= expr between_op expr AND expr.
type Between struct {
	exprBase
	X       Expr
	Not     Token
	Between Token
	Low     Expr
	And     Token
	High    Expr
}

// InList is X [NOT] IN (list), where the list can be empty.
//
// parse.y: in_op ::= IN. in_op ::= NOT IN. expr ::= expr in_op LP exprlist RP.
type InList struct {
	exprBase
	X      Expr
	Not    Token
	In     Token
	LParen Token
	List   []Expr
	Commas []Token
	RParen Token
}

// InSelect is X [NOT] IN (SELECT ...).
//
// parse.y: expr ::= expr in_op LP select RP.
type InSelect struct {
	exprBase
	X      Expr
	Not    Token
	In     Token
	LParen Token
	Select *Select
	RParen Token
}

// InTable is X [NOT] IN [schema.]table, or X [NOT] IN [schema.]function(args), where the arguments
// are in parentheses and can be empty.
//
// parse.y: expr ::= expr in_op nm dbnm paren_exprlist. paren_exprlist ::= LP exprlist RP.
type InTable struct {
	exprBase
	X      Expr
	Not    Token
	In     Token
	Name   QualifiedName
	LParen Token
	Args   []Expr
	Commas []Token
	RParen Token
}

// Collate is X COLLATE name.
//
// parse.y: expr ::= expr COLLATE ids.
type Collate struct {
	exprBase
	X       Expr
	Collate Token
	Name    Token
}

// Cast is CAST(X AS type), where the type can be empty.
//
// parse.y: expr ::= CAST LP expr AS typetoken RP.
type Cast struct {
	exprBase
	Cast   Token
	LParen Token
	X      Expr
	As     Token
	Type   *TypeName
	RParen Token
}

// FuncCall is a call of a function by name: name(*), or name([DISTINCT|ALL] args [ORDER BY ...]),
// with an optional FILTER clause and an optional OVER clause.
//
// parse.y: expr ::= idj LP distinct exprlist RP. expr ::= idj LP distinct exprlist ORDER BY
// sortlist RP. expr ::= idj LP STAR RP. and each of them with filter_over.
type FuncCall struct {
	exprBase
	Name     Token
	LParen   Token
	Distinct Token // DISTINCT or ALL
	Star     Token
	Args     []Expr
	Commas   []Token
	OrderBy  *OrderBy
	RParen   Token
	Filter   *Filter
	Over     *Over
}

// Filter is FILTER (WHERE X) after a function call.
//
// parse.y: filter_clause ::= FILTER LP WHERE expr RP.
type Filter struct {
	base
	Filter Token
	LParen Token
	Where  Token
	X      Expr
	RParen Token
}

// Over is OVER name or OVER (window) after a function call. The window in parentheses can be empty,
// and Window is then nil.
//
// parse.y: over_clause ::= OVER LP window RP. over_clause ::= OVER nm.
type Over struct {
	base
	Over   Token
	Name   Token
	LParen Token
	Window *Window
	RParen Token
}

// Case is CASE [operand] WHEN ... THEN ... [ELSE ...] END.
//
// parse.y: expr ::= CASE case_operand case_exprlist case_else END.
type Case struct {
	exprBase
	Case    Token
	Operand Expr
	Whens   []*When
	Else    Token
	ElseX   Expr
	End     Token
}

// When is one WHEN ... THEN ... of a [Case].
//
// parse.y: case_exprlist ::= case_exprlist WHEN expr THEN expr. case_exprlist ::= WHEN expr THEN
// expr.
type When struct {
	base
	When      Token
	Condition Expr
	Then      Token
	Result    Expr
}

// Raise is RAISE(IGNORE) or RAISE(ROLLBACK|ABORT|FAIL, message).
//
// parse.y: expr ::= RAISE LP IGNORE RP. expr ::= RAISE LP raisetype COMMA expr RP.
type Raise struct {
	exprBase
	Raise   Token
	LParen  Token
	Action  Token
	Comma   Token
	Message Expr
	RParen  Token
}

// QualifiedName is a name with an optional schema before it: [schema.]name. It is not a node; its
// tokens belong to the node that holds it. When Dot is absent, Schema is absent too.
//
// parse.y: nm dbnm, fullname.
type QualifiedName struct {
	Schema Token
	Dot    Token
	Name   Token
}

// Present reports whether the name was written.
func (q QualifiedName) Present() bool {
	return q.Name.Present()
}
