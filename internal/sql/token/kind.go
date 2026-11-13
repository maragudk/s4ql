package token

import "fmt"

// Kind of a token. There is one kind for each token type that sqlite3GetToken gives, and each kind
// is named after its TK_ code in SQLite: [Kind.String] gives that name without the TK_ prefix. Some
// kinds stand for several words, as in SQLite: [LikeKw] is LIKE, GLOB, and REGEXP; [JoinKw] is CROSS,
// FULL, INNER, LEFT, NATURAL, OUTER, and RIGHT; [CTimeKw] is CURRENT_DATE, CURRENT_TIME, and
// CURRENT_TIMESTAMP; [Temp] is TEMP and TEMPORARY. The raw text of the token tells them apart.
type Kind uint8

// Kinds that are not keywords.
const (
	Illegal  Kind = iota // a byte sequence that is no token, and the end of the input
	Space                // whitespace, and a byte order mark where a token starts
	Comment              // -- to the end of the line, or /* to */
	ID                   // a name, bare or quoted with "", ``, or []
	String               // '...'
	Blob                 // x'...'
	Integer              // decimal or hexadecimal
	Float                // with a fraction or an exponent
	QNumber              // a number with a digit separator _ in it
	Variable             // ?, ?NNN, :name, @name, #name, $name
	Semi                 // ;
	LP                   // (
	RP                   // )
	Comma                // ,
	Dot                  // .
	Plus                 // +
	Minus                // -
	Star                 // *
	Slash                // /
	Rem                  // %
	Concat               // ||
	Ptr                  // -> and ->>
	BitAnd               // &
	BitOr                // |
	BitNot               // ~
	LShift               // <<
	RShift               // >>
	EQ                   // = and ==
	NE                   // <> and !=
	LT                   // <
	LE                   // <=
	GT                   // >
	GE                   // >=
)

// Kinds of keywords.
const (
	Abort Kind = iota + GE + 1
	Action
	Add
	After
	All
	Alter
	Always
	Analyze
	And
	As
	Asc
	Attach
	Autoincr // AUTOINCREMENT
	Before
	Begin
	Between
	By
	Cascade
	Case
	Cast
	Check
	Collate
	ColumnKw // COLUMN
	Commit
	Conflict
	Constraint
	Create
	CTimeKw // CURRENT_DATE, CURRENT_TIME, CURRENT_TIMESTAMP
	Current
	Database
	Default
	Deferrable
	Deferred
	Delete
	Desc
	Detach
	Distinct
	Do
	Drop
	Each
	Else
	End
	Escape
	Except
	Exclude
	Exclusive
	Exists
	Explain
	Fail
	Filter
	First
	Following
	For
	Foreign
	From
	Generated
	Group
	Groups
	Having
	If
	Ignore
	Immediate
	In
	Index
	Indexed
	Initially
	Insert
	Instead
	Intersect
	Into
	Is
	IsNull
	Join
	JoinKw // CROSS, FULL, INNER, LEFT, NATURAL, OUTER, RIGHT
	Key
	Last
	LikeKw // LIKE, GLOB, REGEXP
	Limit
	Match
	Materialized
	No
	Not
	Nothing
	NotNull
	Null
	Nulls
	Of
	Offset
	On
	Or
	Order
	Others
	Over
	Partition
	Plan
	Pragma
	Preceding
	Primary
	Query
	Raise
	Range
	Recursive
	References
	Reindex
	Release
	Rename
	Replace
	Restrict
	Returning
	Rollback
	Row
	Rows
	Savepoint
	Select
	Set
	Table
	Temp // TEMP, TEMPORARY
	Then
	Ties
	To
	Transaction
	Trigger
	Unbounded
	Union
	Unique
	Update
	Using
	Vacuum
	Values
	View
	Virtual
	When
	Where
	Window
	With
	Without

	// kinds is the number of kinds. It must stay last.
	kinds
)

// names of the kinds, as SQLite's TK_ codes without the prefix.
var names = [kinds]string{
	Illegal: "ILLEGAL", Space: "SPACE", Comment: "COMMENT", ID: "ID", String: "STRING", Blob: "BLOB",
	Integer: "INTEGER", Float: "FLOAT", QNumber: "QNUMBER", Variable: "VARIABLE", Semi: "SEMI",
	LP: "LP", RP: "RP", Comma: "COMMA", Dot: "DOT", Plus: "PLUS", Minus: "MINUS", Star: "STAR",
	Slash: "SLASH", Rem: "REM", Concat: "CONCAT", Ptr: "PTR", BitAnd: "BITAND", BitOr: "BITOR",
	BitNot: "BITNOT", LShift: "LSHIFT", RShift: "RSHIFT", EQ: "EQ", NE: "NE", LT: "LT", LE: "LE",
	GT: "GT", GE: "GE",

	Abort: "ABORT", Action: "ACTION", Add: "ADD", After: "AFTER", All: "ALL", Alter: "ALTER",
	Always: "ALWAYS", Analyze: "ANALYZE", And: "AND", As: "AS", Asc: "ASC", Attach: "ATTACH",
	Autoincr: "AUTOINCR", Before: "BEFORE", Begin: "BEGIN", Between: "BETWEEN", By: "BY",
	Cascade: "CASCADE", Case: "CASE", Cast: "CAST", Check: "CHECK", Collate: "COLLATE",
	ColumnKw: "COLUMNKW", Commit: "COMMIT", Conflict: "CONFLICT", Constraint: "CONSTRAINT",
	Create: "CREATE", CTimeKw: "CTIME_KW", Current: "CURRENT", Database: "DATABASE",
	Default: "DEFAULT", Deferrable: "DEFERRABLE", Deferred: "DEFERRED", Delete: "DELETE",
	Desc: "DESC", Detach: "DETACH", Distinct: "DISTINCT", Do: "DO", Drop: "DROP", Each: "EACH",
	Else: "ELSE", End: "END", Escape: "ESCAPE", Except: "EXCEPT", Exclude: "EXCLUDE",
	Exclusive: "EXCLUSIVE", Exists: "EXISTS", Explain: "EXPLAIN", Fail: "FAIL", Filter: "FILTER",
	First: "FIRST", Following: "FOLLOWING", For: "FOR", Foreign: "FOREIGN", From: "FROM",
	Generated: "GENERATED", Group: "GROUP", Groups: "GROUPS", Having: "HAVING", If: "IF",
	Ignore: "IGNORE", Immediate: "IMMEDIATE", In: "IN", Index: "INDEX", Indexed: "INDEXED",
	Initially: "INITIALLY", Insert: "INSERT", Instead: "INSTEAD", Intersect: "INTERSECT",
	Into: "INTO", Is: "IS", IsNull: "ISNULL", Join: "JOIN", JoinKw: "JOIN_KW", Key: "KEY",
	Last: "LAST", LikeKw: "LIKE_KW", Limit: "LIMIT", Match: "MATCH", Materialized: "MATERIALIZED",
	No: "NO", Not: "NOT", Nothing: "NOTHING", NotNull: "NOTNULL", Null: "NULL", Nulls: "NULLS",
	Of: "OF", Offset: "OFFSET", On: "ON", Or: "OR", Order: "ORDER", Others: "OTHERS", Over: "OVER",
	Partition: "PARTITION", Plan: "PLAN", Pragma: "PRAGMA", Preceding: "PRECEDING",
	Primary: "PRIMARY", Query: "QUERY", Raise: "RAISE", Range: "RANGE", Recursive: "RECURSIVE",
	References: "REFERENCES", Reindex: "REINDEX", Release: "RELEASE", Rename: "RENAME",
	Replace: "REPLACE", Restrict: "RESTRICT", Returning: "RETURNING", Rollback: "ROLLBACK",
	Row: "ROW", Rows: "ROWS", Savepoint: "SAVEPOINT", Select: "SELECT", Set: "SET",
	Table: "TABLE", Temp: "TEMP", Then: "THEN", Ties: "TIES", To: "TO",
	Transaction: "TRANSACTION", Trigger: "TRIGGER", Unbounded: "UNBOUNDED", Union: "UNION",
	Unique: "UNIQUE", Update: "UPDATE", Using: "USING", Vacuum: "VACUUM", Values: "VALUES",
	View: "VIEW", Virtual: "VIRTUAL", When: "WHEN", Where: "WHERE", Window: "WINDOW", With: "WITH",
	Without: "WITHOUT",
}

// String of the kind: the name of its TK_ code in SQLite, without the prefix.
func (k Kind) String() string {
	if k >= kinds {
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
	return names[k]
}

// IsKeyword reports whether the kind is that of a keyword.
func (k Kind) IsKeyword() bool {
	return k >= Abort && k < kinds
}

// keywords of SQL in upper case, with their kinds: the 147 words of keywordhash.h in tokenize.c,
// listed in the order of its testcase lines, which number them from 1 to 147.
var keywords = map[string]Kind{
	"REINDEX": Reindex, "INDEXED": Indexed, "INDEX": Index, "DESC": Desc, "ESCAPE": Escape,
	"EACH": Each, "CHECK": Check, "KEY": Key, "BEFORE": Before, "FOREIGN": Foreign,
	"FOR": For, "IGNORE": Ignore, "REGEXP": LikeKw, "EXPLAIN": Explain, "INSTEAD": Instead,
	"ADD": Add, "DATABASE": Database, "AS": As, "SELECT": Select, "TABLE": Table,
	"LEFT": JoinKw, "THEN": Then, "END": End, "DEFERRABLE": Deferrable, "ELSE": Else,
	"EXCLUDE": Exclude, "DELETE": Delete, "TEMPORARY": Temp, "TEMP": Temp, "OR": Or,
	"ISNULL": IsNull, "NULLS": Nulls, "SAVEPOINT": Savepoint, "INTERSECT": Intersect, "TIES": Ties,
	"NOTNULL": NotNull, "NOT": Not, "NO": No, "NULL": Null, "LIKE": LikeKw,
	"EXCEPT": Except, "TRANSACTION": Transaction, "ACTION": Action, "ON": On, "NATURAL": JoinKw,
	"ALTER": Alter, "RAISE": Raise, "EXCLUSIVE": Exclusive, "EXISTS": Exists, "CONSTRAINT": Constraint,
	"INTO": Into, "OFFSET": Offset, "OF": Of, "SET": Set, "TRIGGER": Trigger,
	"RANGE": Range, "GENERATED": Generated, "DETACH": Detach, "HAVING": Having, "GLOB": LikeKw,
	"BEGIN": Begin, "INNER": JoinKw, "REFERENCES": References, "UNIQUE": Unique, "QUERY": Query,
	"WITHOUT": Without, "WITH": With, "OUTER": JoinKw, "RELEASE": Release, "ATTACH": Attach,
	"BETWEEN": Between, "NOTHING": Nothing, "GROUPS": Groups, "GROUP": Group, "CASCADE": Cascade,
	"ASC": Asc, "DEFAULT": Default, "CASE": Case, "COLLATE": Collate, "CREATE": Create,
	"CURRENT_DATE": CTimeKw, "IMMEDIATE": Immediate, "JOIN": Join, "INSERT": Insert, "MATCH": Match,
	"PLAN": Plan, "ANALYZE": Analyze, "PRAGMA": Pragma, "MATERIALIZED": Materialized, "DEFERRED": Deferred,
	"DISTINCT": Distinct, "IS": Is, "UPDATE": Update, "VALUES": Values, "VIRTUAL": Virtual,
	"ALWAYS": Always, "WHEN": When, "WHERE": Where, "RECURSIVE": Recursive, "ABORT": Abort,
	"AFTER": After, "RENAME": Rename, "AND": And, "DROP": Drop, "PARTITION": Partition,
	"AUTOINCREMENT": Autoincr, "TO": To, "IN": In, "CAST": Cast, "COLUMN": ColumnKw,
	"COMMIT": Commit, "CONFLICT": Conflict, "CROSS": JoinKw, "CURRENT_TIMESTAMP": CTimeKw, "CURRENT_TIME": CTimeKw,
	"CURRENT": Current, "PRECEDING": Preceding, "FAIL": Fail, "LAST": Last, "FILTER": Filter,
	"REPLACE": Replace, "FIRST": First, "FOLLOWING": Following, "FROM": From, "FULL": JoinKw,
	"LIMIT": Limit, "IF": If, "ORDER": Order, "RESTRICT": Restrict, "OTHERS": Others,
	"OVER": Over, "RETURNING": Returning, "RIGHT": JoinKw, "ROLLBACK": Rollback, "ROWS": Rows,
	"ROW": Row, "UNBOUNDED": Unbounded, "UNION": Union, "USING": Using, "VACUUM": Vacuum,
	"VIEW": View, "WINDOW": Window, "DO": Do, "BY": By, "INITIALLY": Initially,
	"ALL": All, "PRIMARY": Primary,
}
