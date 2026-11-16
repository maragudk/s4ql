package ast

// Statement is one statement of a text: an optional EXPLAIN or EXPLAIN QUERY PLAN, the command, and
// the semicolon that ends it, which is absent when the statement ends at the end of the text.
//
// parse.y: ecmd ::= cmdx SEMI. ecmd ::= explain cmdx SEMI. explain ::= EXPLAIN.
// explain ::= EXPLAIN QUERY PLAN. cmdx ::= cmd.
type Statement struct {
	base
	Explain Token
	Query   Token
	Plan    Token
	Cmd     Cmd
	Semi    Token
}

// Cmd is a command: the cmd of parse.y. Some kinds of command are also the steps of a trigger.
type Cmd interface {
	Node
	cmd()
}

// cmdBase of every command.
type cmdBase struct{ base }

func (*cmdBase) cmd() {}

// Begin is BEGIN [DEFERRED|IMMEDIATE|EXCLUSIVE] [TRANSACTION [name]].
//
// parse.y: cmd ::= BEGIN transtype trans_opt. trans_opt ::= TRANSACTION. trans_opt ::= TRANSACTION
// nm.
type Begin struct {
	cmdBase
	Begin       Token
	Mode        Token
	Transaction Token
	Name        Token
}

// Commit is COMMIT or END, [TRANSACTION [name]].
//
// parse.y: cmd ::= COMMIT|END trans_opt.
type Commit struct {
	cmdBase
	Commit      Token // COMMIT or END
	Transaction Token
	Name        Token
}

// Rollback is ROLLBACK [TRANSACTION [name]] [TO [SAVEPOINT] name].
//
// parse.y: cmd ::= ROLLBACK trans_opt. cmd ::= ROLLBACK trans_opt TO savepoint_opt nm.
type Rollback struct {
	cmdBase
	Rollback        Token
	Transaction     Token
	TransactionName Token
	To              Token
	Savepoint       Token
	SavepointName   Token
}

// Savepoint is SAVEPOINT name.
//
// parse.y: cmd ::= SAVEPOINT nm.
type Savepoint struct {
	cmdBase
	Savepoint Token
	Name      Token
}

// Release is RELEASE [SAVEPOINT] name.
//
// parse.y: cmd ::= RELEASE savepoint_opt nm.
type Release struct {
	cmdBase
	Release   Token
	Savepoint Token
	Name      Token
}

// SelectCmd is a SELECT as a command, or as a step of a trigger.
//
// parse.y: cmd ::= select. trigger_cmd ::= scanpt select scanpt.
type SelectCmd struct {
	cmdBase
	Select *Select
}

// Insert is INSERT or REPLACE. Either Select, or DEFAULT VALUES, is present. As a step of a trigger
// it has no WITH.
//
// parse.y: cmd ::= with insert_cmd INTO xfullname idlist_opt select upsert. cmd ::= with insert_cmd
// INTO xfullname idlist_opt DEFAULT VALUES returning. insert_cmd ::= INSERT orconf.
// insert_cmd ::= REPLACE. trigger_cmd ::= scanpt insert_cmd INTO xfullname idlist_opt select upsert
// scanpt.
type Insert struct {
	cmdBase
	With       *With
	Insert     Token // INSERT or REPLACE
	Or         Token
	Resolution Token // after OR: ROLLBACK, ABORT, FAIL, IGNORE, or REPLACE
	Into       Token
	Table      *TargetTable
	LParen     Token
	Columns    *IDList
	RParen     Token
	Select     *Select
	Default    Token
	Values     Token
	Upserts    []*Upsert
	Returning  *Returning
}

// Upsert is one ON CONFLICT clause of an [Insert].
//
// parse.y: upsert ::= ON CONFLICT LP sortlist RP where_opt DO UPDATE SET setlist where_opt upsert.
// upsert ::= ON CONFLICT LP sortlist RP where_opt DO NOTHING upsert. upsert ::= ON CONFLICT DO
// NOTHING returning. upsert ::= ON CONFLICT DO UPDATE SET setlist where_opt returning.
type Upsert struct {
	base
	On           Token
	Conflict     Token
	LParen       Token
	Target       []*OrderingTerm
	TargetCommas []Token
	RParen       Token
	TargetWhere  *Where
	Do           Token
	Nothing      Token
	Update       Token
	Set          Token
	Assignments  []*Assignment
	Commas       []Token
	Where        *Where
}

// Update is UPDATE. As a step of a trigger it has no WITH and no RETURNING.
//
// parse.y: cmd ::= with UPDATE orconf xfullname indexed_opt SET setlist from where_opt_ret.
// trigger_cmd ::= UPDATE orconf xfullname tridxby SET setlist from where_opt scanpt.
type Update struct {
	cmdBase
	With        *With
	Update      Token
	Or          Token
	Resolution  Token
	Table       *TargetTable
	Indexed     *IndexedBy
	Set         Token
	Assignments []*Assignment
	Commas      []Token
	From        *From
	Where       *Where
	Returning   *Returning
}

// Assignment is column = value, or (columns) = value, in SET.
//
// parse.y: setlist ::= setlist COMMA nm EQ expr. setlist ::= setlist COMMA LP idlist RP EQ expr.
// setlist ::= nm EQ expr. setlist ::= LP idlist RP EQ expr.
type Assignment struct {
	base
	Column  Token
	LParen  Token
	Columns *IDList
	RParen  Token
	Eq      Token
	X       Expr
}

// Delete is DELETE FROM. As a step of a trigger it has no WITH and no RETURNING.
//
// parse.y: cmd ::= with DELETE FROM xfullname indexed_opt where_opt_ret. trigger_cmd ::= DELETE FROM
// xfullname tridxby where_opt scanpt.
type Delete struct {
	cmdBase
	With      *With
	Delete    Token
	From      Token
	Table     *TargetTable
	Indexed   *IndexedBy
	Where     *Where
	Returning *Returning
}

// TargetTable is the table of INSERT, UPDATE, or DELETE: [schema.]name [AS alias].
//
// parse.y: xfullname ::= nm. xfullname ::= nm DOT nm. xfullname ::= nm AS nm.
// xfullname ::= nm DOT nm AS nm.
type TargetTable struct {
	base
	Name  QualifiedName
	As    Token
	Alias Token
}

// Pragma is PRAGMA [schema.]name, with an optional value after = or in parentheses.
//
// parse.y: cmd ::= PRAGMA nm dbnm. cmd ::= PRAGMA nm dbnm EQ nmnum. cmd ::= PRAGMA nm dbnm LP nmnum
// RP. cmd ::= PRAGMA nm dbnm EQ minus_num. cmd ::= PRAGMA nm dbnm LP minus_num RP.
type Pragma struct {
	cmdBase
	Pragma Token
	Name   QualifiedName
	Eq     Token
	LParen Token
	Value  *PragmaValue
	RParen Token
}

// PragmaValue is the value of a [Pragma]: a number with an optional sign, a name, a string, ON,
// DELETE, or DEFAULT.
//
// parse.y: nmnum ::= plus_num. nmnum ::= nm. nmnum ::= ON. nmnum ::= DELETE. nmnum ::= DEFAULT.
// plus_num ::= PLUS number. plus_num ::= number. minus_num ::= MINUS number.
type PragmaValue struct {
	base
	Sign  Token
	Value Token
}

// Vacuum is VACUUM [schema] [INTO file].
//
// parse.y: cmd ::= VACUUM vinto. cmd ::= VACUUM nm vinto. vinto ::= INTO expr.
type Vacuum struct {
	cmdBase
	Vacuum Token
	Schema Token
	Into   Token
	File   Expr
}

// Attach is ATTACH [DATABASE] file AS name [KEY key].
//
// parse.y: cmd ::= ATTACH database_kw_opt expr AS expr key_opt. key_opt ::= KEY expr.
type Attach struct {
	cmdBase
	Attach   Token
	Database Token
	File     Expr
	As       Token
	Name     Expr
	Key      Token
	KeyX     Expr
}

// Detach is DETACH [DATABASE] name.
//
// parse.y: cmd ::= DETACH database_kw_opt expr.
type Detach struct {
	cmdBase
	Detach   Token
	Database Token
	Name     Expr
}

// Reindex is REINDEX [[schema.]name].
//
// parse.y: cmd ::= REINDEX. cmd ::= REINDEX nm dbnm.
type Reindex struct {
	cmdBase
	Reindex Token
	Name    QualifiedName
}

// Analyze is ANALYZE [[schema.]name].
//
// parse.y: cmd ::= ANALYZE. cmd ::= ANALYZE nm dbnm.
type Analyze struct {
	cmdBase
	Analyze Token
	Name    QualifiedName
}
