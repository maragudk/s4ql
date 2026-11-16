package ast

// Select is a SELECT or VALUES, with an optional WITH clause, and the compound operators that join
// its parts. The parts are as written, from left to right: each part keeps its own ORDER BY and LIMIT
// where they were written, so an ORDER BY or LIMIT that is written before a compound operator stays
// on the part before it.
//
// parse.y: select ::= WITH wqlist selectnowith. select ::= WITH RECURSIVE wqlist selectnowith.
// select ::= selectnowith. selectnowith ::= oneselect. selectnowith ::= selectnowith multiselect_op
// oneselect.
type Select struct {
	base
	With      *With
	First     SelectCore
	Compounds []*Compound
}

// Compound is a compound operator and the part of a [Select] after it: UNION, UNION ALL, EXCEPT, or
// INTERSECT.
//
// parse.y: multiselect_op ::= UNION. multiselect_op ::= UNION ALL.
// multiselect_op ::= EXCEPT|INTERSECT.
type Compound struct {
	base
	Op   Token
	All  Token
	Core SelectCore
}

// SelectCore is one part of a [Select]: a [SimpleSelect] or [Values].
type SelectCore interface {
	Node
	selectCore()
}

// SimpleSelect is SELECT with its clauses.
//
// parse.y: oneselect ::= SELECT distinct selcollist from where_opt groupby_opt having_opt
// orderby_opt limit_opt. oneselect ::= SELECT distinct selcollist from where_opt groupby_opt
// having_opt window_clause orderby_opt limit_opt.
type SimpleSelect struct {
	base
	Select   Token
	Distinct Token // DISTINCT or ALL
	Columns  []*ResultColumn
	Commas   []Token
	From     *From
	Where    *Where
	GroupBy  *GroupBy
	Having   *Having
	Window   *WindowClause
	OrderBy  *OrderBy
	Limit    *Limit
}

func (*SimpleSelect) selectCore() {}

// Values is VALUES with one or more rows.
//
// parse.y: values ::= VALUES LP nexprlist RP. mvalues ::= values COMMA LP nexprlist RP.
// mvalues ::= mvalues COMMA LP nexprlist RP.
type Values struct {
	base
	Values Token
	Rows   []*ValuesRow
	Commas []Token
}

func (*Values) selectCore() {}

// ValuesRow is one row of [Values].
type ValuesRow struct {
	base
	LParen Token
	Exprs  []Expr
	Commas []Token
	RParen Token
}

// ResultColumn is one column of a SELECT or a RETURNING clause: an expression with an optional alias,
// *, or table.*.
//
// parse.y: selcollist ::= sclp scanpt expr scanpt as. selcollist ::= sclp scanpt STAR.
// selcollist ::= sclp scanpt nm DOT STAR. as ::= AS nm. as ::= ids.
type ResultColumn struct {
	base
	Expr  Expr
	As    Token
	Alias Token
	Table Token
	Dot   Token
	Star  Token
}

// From is FROM and the items it joins.
//
// parse.y: from ::= FROM seltablist.
type From struct {
	base
	From  Token
	Items []*FromItem
}

// FromItem is one item of a FROM clause, or of a join in parentheses: the join operator before it,
// except on the first item, then the source, an optional alias, INDEXED BY or NOT INDEXED, and ON or
// USING. Any of these can be absent, ON and USING on the first item too, as in the grammar.
//
// parse.y: stl_prefix ::= seltablist joinop. seltablist ::= stl_prefix nm dbnm as on_using.
// seltablist ::= stl_prefix nm dbnm as indexed_by on_using. seltablist ::= stl_prefix nm dbnm LP
// exprlist RP as on_using. seltablist ::= stl_prefix LP select RP as on_using.
// seltablist ::= stl_prefix LP seltablist RP as on_using.
type FromItem struct {
	base
	Join       *JoinOp
	Source     FromSource
	As         Token
	Alias      Token
	Indexed    *IndexedBy
	Constraint JoinConstraint
}

// FromSource is what a [FromItem] reads from.
type FromSource interface {
	Node
	fromSource()
}

// TableSource is a table or view by name.
type TableSource struct {
	base
	Name QualifiedName
}

func (*TableSource) fromSource() {}

// TableFunction is a table-valued function with its arguments, which can be empty.
type TableFunction struct {
	base
	Name   QualifiedName
	LParen Token
	Args   []Expr
	Commas []Token
	RParen Token
}

func (*TableFunction) fromSource() {}

// SubquerySource is a SELECT in parentheses.
type SubquerySource struct {
	base
	LParen Token
	Select *Select
	RParen Token
}

func (*SubquerySource) fromSource() {}

// NestedFrom is a list of joined items in parentheses.
type NestedFrom struct {
	base
	LParen Token
	Items  []*FromItem
	RParen Token
}

func (*NestedFrom) fromSource() {}

// JoinOp is the operator that joins a [FromItem] to the items before it: a comma, JOIN, or one to
// three words and JOIN, as in LEFT OUTER JOIN. The first word is a join keyword, the others are
// names as the grammar reads them; which words make a valid join type is not the parser's to say.
//
// parse.y: joinop ::= COMMA|JOIN. joinop ::= JOIN_KW JOIN. joinop ::= JOIN_KW nm JOIN.
// joinop ::= JOIN_KW nm nm JOIN.
type JoinOp struct {
	base
	Comma Token
	Words []Token
	Join  Token
}

// JoinConstraint is ON or USING after a [FromItem].
type JoinConstraint interface {
	Node
	joinConstraint()
}

// JoinOn is ON and an expression.
//
// parse.y: on_using ::= ON expr.
type JoinOn struct {
	base
	On Token
	X  Expr
}

func (*JoinOn) joinConstraint() {}

// JoinUsing is USING and a list of columns in parentheses.
//
// parse.y: on_using ::= USING LP idlist RP.
type JoinUsing struct {
	base
	Using   Token
	LParen  Token
	Columns *IDList
	RParen  Token
}

func (*JoinUsing) joinConstraint() {}

// IndexedBy is INDEXED BY name or NOT INDEXED.
//
// parse.y: indexed_by ::= INDEXED BY nm. indexed_by ::= NOT INDEXED. tridxby ::= INDEXED BY nm.
// tridxby ::= NOT INDEXED.
type IndexedBy struct {
	base
	Not     Token
	Indexed Token
	By      Token
	Index   Token
}

// Where is WHERE and an expression.
//
// parse.y: where_opt ::= WHERE expr. where_opt_ret ::= WHERE expr.
type Where struct {
	base
	Where Token
	X     Expr
}

// GroupBy is GROUP BY and its expressions.
//
// parse.y: groupby_opt ::= GROUP BY nexprlist.
type GroupBy struct {
	base
	Group  Token
	By     Token
	Exprs  []Expr
	Commas []Token
}

// Having is HAVING and an expression.
//
// parse.y: having_opt ::= HAVING expr.
type Having struct {
	base
	Having Token
	X      Expr
}

// OrderBy is ORDER BY and its terms.
//
// parse.y: orderby_opt ::= ORDER BY sortlist.
type OrderBy struct {
	base
	Order  Token
	By     Token
	Terms  []*OrderingTerm
	Commas []Token
}

// OrderingTerm is an expression with an optional ASC or DESC and an optional NULLS FIRST or NULLS
// LAST: an item of a sortlist in parse.y, used in ORDER BY, in the columns of CREATE INDEX, PRIMARY
// KEY, and UNIQUE, and in the target of ON CONFLICT.
//
// parse.y: sortlist ::= sortlist COMMA expr sortorder nulls. sortlist ::= expr sortorder nulls.
type OrderingTerm struct {
	base
	X     Expr
	Order Token // ASC or DESC
	Nulls Token
	Which Token // FIRST or LAST
}

// Limit is LIMIT count, LIMIT count OFFSET offset, or LIMIT a, b. The operands are kept in the order
// written: for LIMIT a, b, X is a and Y is b, and Sep is the comma, although b is the count.
//
// parse.y: limit_opt ::= LIMIT expr. limit_opt ::= LIMIT expr OFFSET expr.
// limit_opt ::= LIMIT expr COMMA expr.
type Limit struct {
	base
	Limit Token
	X     Expr
	Sep   Token // OFFSET or a comma
	Y     Expr
}

// With is a WITH clause and its common table expressions.
//
// parse.y: with ::= WITH wqlist. with ::= WITH RECURSIVE wqlist. wqlist ::= wqitem.
// wqlist ::= wqlist COMMA wqitem.
type With struct {
	base
	With      Token
	Recursive Token
	CTEs      []*CTE
	Commas    []Token
}

// CTE is one common table expression: name [(columns)] AS [[NOT] MATERIALIZED] (select).
//
// parse.y: wqitem ::= withnm eidlist_opt wqas LP select RP. wqas ::= AS. wqas ::= AS MATERIALIZED.
// wqas ::= AS NOT MATERIALIZED.
type CTE struct {
	base
	Name         Token
	Columns      *ColumnNames
	As           Token
	Not          Token
	Materialized Token
	LParen       Token
	Select       *Select
	RParen       Token
}

// ColumnNames is a list of column names in parentheses, where each name can have a COLLATE and an
// ASC or DESC after it. The grammar reads them so that old schemas still load; outside those, SQLite
// rejects them in an action, after the parse.
//
// parse.y: eidlist_opt ::= LP eidlist RP. eidlist ::= eidlist COMMA nm collate sortorder.
// eidlist ::= nm collate sortorder.
type ColumnNames struct {
	base
	LParen  Token
	Columns []*ColumnName
	Commas  []Token
	RParen  Token
}

// ColumnName is one name of [ColumnNames].
//
// parse.y: eidlist ::= nm collate sortorder. collate ::= COLLATE ids.
type ColumnName struct {
	base
	Name      Token
	Collate   Token
	Collation Token
	Order     Token
}

// IDList is a list of names separated by commas, without parentheses: the idlist of parse.y.
//
// parse.y: idlist ::= idlist COMMA nm. idlist ::= nm.
type IDList struct {
	base
	Names  []Token
	Commas []Token
}

// WindowClause is WINDOW and its definitions.
//
// parse.y: window_clause ::= WINDOW windowdefn_list. windowdefn_list ::= windowdefn.
// windowdefn_list ::= windowdefn_list COMMA windowdefn.
type WindowClause struct {
	base
	Window Token
	Defs   []*WindowDef
	Commas []Token
}

// WindowDef is name AS (window). The window can be empty, and Window is then nil.
//
// parse.y: windowdefn ::= nm AS LP window RP.
type WindowDef struct {
	base
	Name   Token
	As     Token
	LParen Token
	Window *Window
	RParen Token
}

// Window is the inside of the parentheses of a window: an optional base window name, PARTITION BY,
// ORDER BY, and a frame, each of them optional.
//
// parse.y: window ::= PARTITION BY nexprlist orderby_opt frame_opt. window ::= nm PARTITION BY
// nexprlist orderby_opt frame_opt. window ::= ORDER BY sortlist frame_opt. window ::= nm ORDER BY
// sortlist frame_opt. window ::= frame_opt. window ::= nm frame_opt.
type Window struct {
	base
	Base            Token
	Partition       Token
	PartitionBy     Token
	PartitionExprs  []Expr
	PartitionCommas []Token
	OrderBy         *OrderBy
	Frame           *Frame
}

// Frame is the frame of a [Window]: RANGE, ROWS, or GROUPS, a start bound or BETWEEN two bounds, and
// an optional EXCLUDE.
//
// parse.y: frame_opt ::= range_or_rows frame_bound_s frame_exclude_opt. frame_opt ::=
// range_or_rows BETWEEN frame_bound_s AND frame_bound_e frame_exclude_opt. frame_exclude_opt ::=
// EXCLUDE frame_exclude.
type Frame struct {
	base
	Unit    Token // RANGE, ROWS, or GROUPS
	Between Token
	Start   *FrameBound
	And     Token
	End     *FrameBound
	Exclude Token
	// Excluded is what EXCLUDE excludes, as written: NO OTHERS, CURRENT ROW, GROUP, or TIES.
	Excluded []Token
}

// FrameBound is one bound of a [Frame]: UNBOUNDED PRECEDING, UNBOUNDED FOLLOWING, CURRENT ROW, or
// an expression and PRECEDING or FOLLOWING.
//
// parse.y: frame_bound_s ::= frame_bound. frame_bound_s ::= UNBOUNDED PRECEDING.
// frame_bound_e ::= frame_bound. frame_bound_e ::= UNBOUNDED FOLLOWING. frame_bound ::= expr
// PRECEDING|FOLLOWING. frame_bound ::= CURRENT ROW.
type FrameBound struct {
	base
	Unbounded Token
	Current   Token
	Row       Token
	X         Expr
	Direction Token // PRECEDING or FOLLOWING
}

// Returning is RETURNING and its columns.
//
// parse.y: returning ::= RETURNING selcollist. where_opt_ret ::= RETURNING selcollist.
// upsert ::= RETURNING selcollist.
type Returning struct {
	base
	Returning Token
	Columns   []*ResultColumn
	Commas    []Token
}
