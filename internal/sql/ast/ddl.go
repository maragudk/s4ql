package ast

// IfNotExists is IF NOT EXISTS, or IF EXISTS when Not is absent. It is not a node; its tokens belong
// to the node that holds it.
//
// parse.y: ifnotexists ::= IF NOT EXISTS. ifexists ::= IF EXISTS.
type IfNotExists struct {
	If     Token
	Not    Token
	Exists Token
}

// CreateTable is CREATE [TEMP] TABLE, with columns and constraints in parentheses and table options
// after them, or with AS and a SELECT.
//
// parse.y: cmd ::= create_table create_table_args. create_table ::= createkw temp TABLE ifnotexists
// nm dbnm. create_table_args ::= LP columnlist conslist_opt RP table_option_set.
// create_table_args ::= AS select.
type CreateTable struct {
	cmdBase
	Create      Token
	Temp        Token // TEMP or TEMPORARY
	Table       Token
	IfNotExists IfNotExists
	Name        QualifiedName
	LParen      Token
	Columns     []*ColumnDef
	Constraints []TableConstraint
	// Commas between the columns and constraints, in the order written. Between two table
	// constraints the comma can be left out.
	Commas        []Token
	RParen Token
	// Options after the parentheses, and the commas between them. The grammar allows the first
	// option to be empty, so a comma can come right after the parenthesis, before the first option.
	Options       []*TableOption
	OptionCommas  []Token
	As            Token
	Select        *Select
}

// TableOption is WITHOUT name, or a name, after the parentheses of [CreateTable]. Which names are
// options, such as WITHOUT ROWID and STRICT, is not the parser's to say.
//
// parse.y: table_option ::= WITHOUT nm. table_option ::= nm.
type TableOption struct {
	base
	Without Token
	Name    Token
}

// ColumnDef is a column of [CreateTable] or of ALTER TABLE ... ADD: a name, an optional type, and
// its constraints.
//
// parse.y: columnname ::= nm typetoken. columnlist ::= columnname carglist.
// carglist ::= carglist ccons.
type ColumnDef struct {
	base
	Name        Token
	Type        *TypeName
	Constraints []ColumnConstraint
}

// TypeName is the declared type of a column or of CAST: one or more names, and optionally one or two
// numbers in parentheses.
//
// parse.y: typetoken ::= typename. typetoken ::= typename LP signed RP. typetoken ::= typename LP
// signed COMMA signed RP. typename ::= ids. typename ::= typename ids.
type TypeName struct {
	base
	Names  []Token
	LParen Token
	Arg1   *SignedNumber
	Comma  Token
	Arg2   *SignedNumber
	RParen Token
}

// SignedNumber is an integer or a real with an optional + or -.
//
// parse.y: signed ::= plus_num. signed ::= minus_num.
type SignedNumber struct {
	base
	Sign   Token
	Number Token
}

// ColumnConstraint is one item of the constraints of a [ColumnDef]: the ccons of parse.y. Each
// item is as written; CONSTRAINT name, for example, is an item of its own, as in the grammar.
type ColumnConstraint interface {
	Node
	columnConstraint()
}

// TableConstraint is one table constraint of [CreateTable]: the tcons of parse.y. CONSTRAINT name
// is an item of its own, as in the grammar.
type TableConstraint interface {
	Node
	tableConstraint()
}

// ConstraintName is CONSTRAINT name, which names the constraint after it.
//
// parse.y: ccons ::= CONSTRAINT nm. tcons ::= CONSTRAINT nm.
type ConstraintName struct {
	base
	Constraint Token
	Name       Token
}

func (*ConstraintName) columnConstraint() {}
func (*ConstraintName) tableConstraint()  {}

// DefaultConstraint is DEFAULT and a value: a literal, a literal after + or -, an expression in
// parentheses, or a name. For a name, Name is present and X is nil.
//
// parse.y: ccons ::= DEFAULT scantok term. ccons ::= DEFAULT LP expr RP. ccons ::= DEFAULT PLUS
// scantok term. ccons ::= DEFAULT MINUS scantok term. ccons ::= DEFAULT scantok id.
type DefaultConstraint struct {
	base
	Default Token
	Sign    Token
	LParen  Token
	X       Expr
	RParen  Token
	Name    Token
}

func (*DefaultConstraint) columnConstraint() {}

// NullConstraint is NULL or NOT NULL, with an optional ON CONFLICT.
//
// parse.y: ccons ::= NULL onconf. ccons ::= NOT NULL onconf.
type NullConstraint struct {
	base
	Not      Token
	Null     Token
	Conflict *OnConflict
}

func (*NullConstraint) columnConstraint() {}

// PrimaryKeyConstraint is PRIMARY KEY on a column, with an optional ASC or DESC, ON CONFLICT, and
// AUTOINCREMENT.
//
// parse.y: ccons ::= PRIMARY KEY sortorder onconf autoinc.
type PrimaryKeyConstraint struct {
	base
	Primary       Token
	Key           Token
	Order         Token
	Conflict      *OnConflict
	Autoincrement Token
}

func (*PrimaryKeyConstraint) columnConstraint() {}

// UniqueConstraint is UNIQUE on a column, with an optional ON CONFLICT.
//
// parse.y: ccons ::= UNIQUE onconf.
type UniqueConstraint struct {
	base
	Unique   Token
	Conflict *OnConflict
}

func (*UniqueConstraint) columnConstraint() {}

// CheckConstraint is CHECK (expression) on a column or a table. Only on a table can it have an ON
// CONFLICT.
//
// parse.y: ccons ::= CHECK LP expr RP. tcons ::= CHECK LP expr RP onconf.
type CheckConstraint struct {
	base
	Check    Token
	LParen   Token
	X        Expr
	RParen   Token
	Conflict *OnConflict
}

func (*CheckConstraint) columnConstraint() {}
func (*CheckConstraint) tableConstraint()  {}

// ReferencesConstraint is REFERENCES on a column. A DEFERRABLE clause after it is a constraint of
// its own, a [DeferConstraint], as in the grammar.
//
// parse.y: ccons ::= REFERENCES nm eidlist_opt refargs.
type ReferencesConstraint struct {
	base
	References *References
}

func (*ReferencesConstraint) columnConstraint() {}

// References is REFERENCES table [(columns)] and its actions: the part that a column's REFERENCES
// and a table's FOREIGN KEY have in common.
//
// parse.y: REFERENCES nm eidlist_opt refargs. refargs ::= refargs refarg.
type References struct {
	base
	References Token
	Table      Token
	Columns    *ColumnNames
	Args       []*RefArg
}

// RefArg is one action of a [References]: MATCH name, or ON INSERT, ON DELETE, or ON UPDATE and
// SET NULL, SET DEFAULT, CASCADE, RESTRICT, or NO ACTION. Action holds the one or two words of the
// action as written.
//
// parse.y: refarg ::= MATCH nm. refarg ::= ON INSERT refact. refarg ::= ON DELETE refact.
// refarg ::= ON UPDATE refact. refact ::= SET NULL. refact ::= SET DEFAULT. refact ::= CASCADE.
// refact ::= RESTRICT. refact ::= NO ACTION.
type RefArg struct {
	base
	Match  Token
	Name   Token
	On     Token
	Event  Token // INSERT, DELETE, or UPDATE
	Action []Token
}

// DeferConstraint is [NOT] DEFERRABLE [INITIALLY DEFERRED|IMMEDIATE] on a column.
//
// parse.y: ccons ::= defer_subclause.
type DeferConstraint struct {
	base
	Defer *Deferrable
}

func (*DeferConstraint) columnConstraint() {}

// Deferrable is [NOT] DEFERRABLE [INITIALLY DEFERRED|IMMEDIATE].
//
// parse.y: defer_subclause ::= NOT DEFERRABLE init_deferred_pred_opt. defer_subclause ::=
// DEFERRABLE init_deferred_pred_opt. init_deferred_pred_opt ::= INITIALLY DEFERRED.
// init_deferred_pred_opt ::= INITIALLY IMMEDIATE.
type Deferrable struct {
	base
	Not        Token
	Deferrable Token
	Initially  Token
	Mode       Token // DEFERRED or IMMEDIATE
}

// CollateConstraint is COLLATE name on a column.
//
// parse.y: ccons ::= COLLATE ids.
type CollateConstraint struct {
	base
	Collate Token
	Name    Token
}

func (*CollateConstraint) columnConstraint() {}

// GeneratedConstraint is [GENERATED ALWAYS] AS (expression) [name] on a column, where the name is
// STORED or VIRTUAL as SQLite allows it, and any name as the grammar reads it.
//
// parse.y: ccons ::= GENERATED ALWAYS AS generated. ccons ::= AS generated. generated ::= LP expr RP.
// generated ::= LP expr RP ID.
type GeneratedConstraint struct {
	base
	Generated Token
	Always    Token
	As        Token
	LParen    Token
	X         Expr
	RParen    Token
	Storage   Token
}

func (*GeneratedConstraint) columnConstraint() {}

// OnConflict is ON CONFLICT and a resolution: ROLLBACK, ABORT, FAIL, IGNORE, or REPLACE.
//
// parse.y: onconf ::= ON CONFLICT resolvetype.
type OnConflict struct {
	base
	On         Token
	Conflict   Token
	Resolution Token
}

// TablePrimaryKey is PRIMARY KEY (columns) on a table, with an optional AUTOINCREMENT inside the
// parentheses and ON CONFLICT after them. The columns are a sortlist, so they can be any expression
// as the grammar reads them.
//
// parse.y: tcons ::= PRIMARY KEY LP sortlist autoinc RP onconf.
type TablePrimaryKey struct {
	base
	Primary       Token
	Key           Token
	LParen        Token
	Columns       []*OrderingTerm
	Commas        []Token
	Autoincrement Token
	RParen        Token
	Conflict      *OnConflict
}

func (*TablePrimaryKey) tableConstraint() {}

// TableUnique is UNIQUE (columns) on a table, with an optional ON CONFLICT.
//
// parse.y: tcons ::= UNIQUE LP sortlist RP onconf.
type TableUnique struct {
	base
	Unique   Token
	LParen   Token
	Columns  []*OrderingTerm
	Commas   []Token
	RParen   Token
	Conflict *OnConflict
}

func (*TableUnique) tableConstraint() {}

// TableForeignKey is FOREIGN KEY (columns) REFERENCES ... on a table, with an optional DEFERRABLE
// clause.
//
// parse.y: tcons ::= FOREIGN KEY LP eidlist RP REFERENCES nm eidlist_opt refargs
// defer_subclause_opt.
type TableForeignKey struct {
	base
	Foreign    Token
	Key        Token
	Columns    *ColumnNames
	References *References
	Defer      *Deferrable
}

func (*TableForeignKey) tableConstraint() {}

// Drop is DROP TABLE, DROP VIEW, DROP INDEX, or DROP TRIGGER, with an optional IF EXISTS. Object is
// the second keyword, which tells which.
//
// parse.y: cmd ::= DROP TABLE ifexists fullname. cmd ::= DROP VIEW ifexists fullname. cmd ::= DROP
// INDEX ifexists fullname. cmd ::= DROP TRIGGER ifexists fullname.
type Drop struct {
	cmdBase
	Drop     Token
	Object   Token
	IfExists IfNotExists
	Name     QualifiedName
}

// CreateView is CREATE [TEMP] VIEW [IF NOT EXISTS] name [(columns)] AS select.
//
// parse.y: cmd ::= createkw temp VIEW ifnotexists nm dbnm eidlist_opt AS select.
type CreateView struct {
	cmdBase
	Create      Token
	Temp        Token
	View        Token
	IfNotExists IfNotExists
	Name        QualifiedName
	Columns     *ColumnNames
	As          Token
	Select      *Select
}

// CreateIndex is CREATE [UNIQUE] INDEX [IF NOT EXISTS] name ON table (columns) [WHERE ...]. The
// columns are a sortlist, so they can be expressions.
//
// parse.y: cmd ::= createkw uniqueflag INDEX ifnotexists nm dbnm ON nm LP sortlist RP where_opt.
type CreateIndex struct {
	cmdBase
	Create      Token
	Unique      Token
	Index       Token
	IfNotExists IfNotExists
	Name        QualifiedName
	On          Token
	Table       Token
	LParen      Token
	Columns     []*OrderingTerm
	Commas      []Token
	RParen      Token
	Where       *Where
}

// CreateTrigger is CREATE [TEMP] TRIGGER with its steps. Each step is an [Update], [Insert],
// [Delete], or [SelectCmd], and the semicolons after them are in Semis.
//
// parse.y: cmd ::= createkw trigger_decl BEGIN trigger_cmd_list END. trigger_decl ::= temp TRIGGER
// ifnotexists nm dbnm trigger_time trigger_event ON fullname foreach_clause when_clause.
// trigger_time ::= BEFORE|AFTER. trigger_time ::= INSTEAD OF. trigger_event ::= DELETE|INSERT.
// trigger_event ::= UPDATE. trigger_event ::= UPDATE OF idlist. foreach_clause ::= FOR EACH ROW.
// when_clause ::= WHEN expr. trigger_cmd_list ::= trigger_cmd_list trigger_cmd SEMI.
// trigger_cmd_list ::= trigger_cmd SEMI.
type CreateTrigger struct {
	cmdBase
	Create      Token
	Temp        Token
	Trigger     Token
	IfNotExists IfNotExists
	Name        QualifiedName
	Time        Token // BEFORE, AFTER, or INSTEAD
	TimeOf      Token // OF after INSTEAD
	Event       Token // DELETE, INSERT, or UPDATE
	EventOf     Token // OF after UPDATE
	Columns     *IDList
	On          Token
	Table       QualifiedName
	For         Token
	Each        Token
	Row         Token
	When        Token
	WhenX       Expr
	Begin       Token
	Steps       []Cmd
	Semis       []Token
	End         Token
}

// CreateVirtualTable is CREATE VIRTUAL TABLE [IF NOT EXISTS] name USING module, with optional
// arguments in parentheses. An argument is any tokens, with parentheses balanced, up to a comma or
// the closing parenthesis.
//
// parse.y: cmd ::= create_vtab. cmd ::= create_vtab LP vtabarglist RP. create_vtab ::= createkw
// VIRTUAL TABLE ifnotexists nm dbnm USING nm. vtabarglist ::= vtabarg. vtabarglist ::= vtabarglist
// COMMA vtabarg.
type CreateVirtualTable struct {
	cmdBase
	Create      Token
	Virtual     Token
	Table       Token
	IfNotExists IfNotExists
	Name        QualifiedName
	Using       Token
	Module      Token
	LParen      Token
	Args        []*ModuleArg
	Commas      []Token
	RParen      Token
}

// ModuleArg is one argument of [CreateVirtualTable]: its tokens as written, parentheses included. An
// empty argument has no tokens, and its span is empty, at the position of the comma or parenthesis
// after it.
//
// parse.y: vtabarg ::= . vtabarg ::= vtabarg vtabargtoken. vtabargtoken ::= ANY. vtabargtoken ::=
// lp anylist RP. anylist ::= anylist LP anylist RP. anylist ::= anylist ANY.
type ModuleArg struct {
	base
	Tokens []Token
}

// AlterTable is ALTER TABLE [schema.]name and an action.
//
// parse.y: cmd ::= ALTER TABLE fullname ..., cmd ::= alter_add carglist.
type AlterTable struct {
	cmdBase
	Alter  Token
	Table  Token
	Name   QualifiedName
	Action AlterAction
}

// AlterAction is what an [AlterTable] does.
type AlterAction interface {
	Node
	alterAction()
}

// RenameTable is RENAME TO name.
//
// parse.y: cmd ::= ALTER TABLE fullname RENAME TO nm.
type RenameTable struct {
	base
	Rename Token
	To     Token
	Name   Token
}

func (*RenameTable) alterAction() {}

// AddColumn is ADD [COLUMN] and a column definition.
//
// parse.y: alter_add ::= ALTER TABLE fullname ADD kwcolumn_opt nm typetoken. cmd ::= alter_add
// carglist.
type AddColumn struct {
	base
	Add    Token
	Column Token
	Def    *ColumnDef
}

func (*AddColumn) alterAction() {}

// DropColumn is DROP [COLUMN] name.
//
// parse.y: cmd ::= ALTER TABLE fullname DROP kwcolumn_opt nm.
type DropColumn struct {
	base
	Drop   Token
	Column Token
	Name   Token
}

func (*DropColumn) alterAction() {}

// RenameColumn is RENAME [COLUMN] old TO new.
//
// parse.y: cmd ::= ALTER TABLE fullname RENAME kwcolumn_opt nm TO nm.
type RenameColumn struct {
	base
	Rename  Token
	Column  Token
	Old     Token
	To      Token
	New     Token
}

func (*RenameColumn) alterAction() {}

// DropConstraint is DROP CONSTRAINT name.
//
// parse.y: cmd ::= ALTER TABLE fullname DROP CONSTRAINT nm.
type DropConstraint struct {
	base
	Drop       Token
	Constraint Token
	Name       Token
}

func (*DropConstraint) alterAction() {}

// AlterColumnNotNull is ALTER [COLUMN] name DROP NOT NULL, or ALTER [COLUMN] name SET NOT NULL with
// an optional ON CONFLICT. Drop or Set tells which.
//
// parse.y: cmd ::= ALTER TABLE fullname ALTER kwcolumn_opt nm DROP NOT NULL. cmd ::= ALTER TABLE
// fullname ALTER kwcolumn_opt nm SET NOT NULL onconf.
type AlterColumnNotNull struct {
	base
	Alter    Token
	Column   Token
	Name     Token
	Drop     Token
	Set      Token
	Not      Token
	Null     Token
	Conflict *OnConflict
}

func (*AlterColumnNotNull) alterAction() {}

// AddCheck is ADD [CONSTRAINT name] CHECK (expression) with an optional ON CONFLICT.
//
// parse.y: cmd ::= ALTER TABLE fullname ADD CONSTRAINT nm CHECK LP expr RP onconf. cmd ::= ALTER
// TABLE fullname ADD CHECK LP expr RP onconf.
type AddCheck struct {
	base
	Add        Token
	Constraint Token
	Name       Token
	Check      *CheckConstraint
}

func (*AddCheck) alterAction() {}
