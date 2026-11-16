# SQL parser and syntax tree

`internal/sql/parser` reads SQL text into the syntax tree of `internal/sql/ast`. It accepts and rejects exactly what SQLite 3.54.1 does, with SQLite's syntax errors and their byte offsets.

The grammar is SQLite's own `src/parse.y`, with the compile options of the binding in [`internal/sqlite`](../../sqlite/README.md). The parser is a hand-written recursive descent for that grammar. Every non-trivial function names the rules of `parse.y` that it reads.

The parser keeps everything as written. It rejects only what SQLite's grammar rejects. A pass after the parser, which is agreed and not built, rejects what s4ql does not support, and raises the errors that SQLite's grammar actions raise. [docs/todo.md](../../../docs/todo.md) describes that pass and the four checks of the parser, of which the first two are built: agreement with SQLite, and the ownership of every token.

SQLite's verdicts come from the oracle in [`internal/sql/oracle`](../oracle/README.md), through the harness in [`internal/sql/parsetest`](../parsetest/README.md).

## The API

Package `parser`:

- `Parse(src)` reads the first statement of a text as `sqlite3_prepare_v2` does: up to the first NUL byte, skipping empty statements. It returns the statement and the tail, which is the offset after the statement's semicolon or the end of the text. A text with no statement gives a nil statement, no error, and the tail at the end.
- `ParseAll(src)` reads each statement from where the last one ended, until the end of the text or the first error.
- `Tokens(src, stmt)` gives the tokens that the parser read for a statement, from the tokenizer. The ownership check compares the tree with them.
- `Error` holds SQLite's `Message` and `Offset`, with -1 where SQLite gives no offset. `Limit` is true for a limit of this parser, whose message starts with `s4ql: `.
- `MaxDepth` is 2500 and `MaxLength` is 1,000,000,000. See [Limits](#limits).

Package `ast`:

- `Node` is the interface of every node, with `Bounds() Span`. `Span` holds `Start` and `End` in bytes.
- `Token` holds `Kind`, `Offset`, and `Text`. The zero value is an absent token. `Present` and `End` are its methods.
- `Children(n)` gives the children of a node, and `Tokens(n)` its own tokens, sorted. `Walk(n, f)` visits a tree, and `AllTokens(n)` gives every token of a tree.
- `Dump(n)` writes a tree as one line of text, for tests.
- `CheckOwnership(root, read)` checks the [ownership rule](#the-ownership-rule).
- `SetSpan(n, s)` is for the parser.

The root of a tree is `ast.Statement`, with the fields `Explain`, `Query`, `Plan`, `Cmd`, and `Semi`. The tree has 113 node types, and the doc comment of each names the rules of `parse.y` that it stands for. Eight interfaces group them: `Expr`, `Cmd`, `SelectCore`, `FromSource`, `JoinConstraint`, `ColumnConstraint`, `TableConstraint`, and `AlterAction`.

## The tree

### It keeps what was written

The tree has the shape of the text, not the shape of SQLite's internal form.

- A name keeps its raw text with its quotes. A literal keeps its raw text. A keyword keeps its spelling.
- `Token.Kind` is always the kind that the tokenizer gives. A keyword read as a name keeps the kind of the keyword, and so do WINDOW, OVER, and FILTER read as names.
- Commas, parentheses, and every keyword are tokens of a node.
- An optional keyword that changes the meaning is a `Token` field of its own, absent when not written: DISTINCT or ALL, ASC or DESC, NULLS FIRST or LAST, TEMP, IF NOT EXISTS, AS before an alias, COLUMN in ALTER TABLE, INDEXED BY or NOT INDEXED, OR with its resolution, the words of a join, MATERIALIZED, and more.
- `Statement.Semi` is absent when the statement ends at the end of the text. The empty semicolon that SQLite feeds its parser there is not a token of the text.

Nothing is rewritten:

| Text | Tree, as `ast.Dump` writes it |
| --- | --- |
| `x NOT NULL` | `NullTest{X: ColumnRef{Column: x}, Op: NOT, Null: NULL}` |
| `x NOTNULL` | `NullTest{X: ColumnRef{Column: x}, Op: NOTNULL}` |
| `x IS NOT NULL` | `Is{X: ColumnRef{Column: x}, Is: IS, Not: NOT, Y: Literal{Value: NULL}}` |
| `(a)` | `Paren{LParen: (, X: ColumnRef{Column: a}, RParen: )}` |
| `count(*)` | `FuncCall{Name: count, LParen: (, Star: *, RParen: )}` |
| `"t"` | `ColumnRef{Column: "t"}` |
| `1_000` | `Literal{Value: 1_000}` |
| `limit 1, 2` | `Limit{Limit: limit, X: Literal{Value: 1}, Sep: ,, Y: Literal{Value: 2}}` |

More cases:

- `-5` is a `Unary` over a `Literal`.
- Each part of a compound select keeps its own ORDER BY and LIMIT where they are written. In `select 1 order by 1 union select 2`, the first part holds the ORDER BY.
- `CONSTRAINT name` is a constraint item of its own, as in the grammar. So is a DEFERRABLE after a column's REFERENCES.

Grouping follows SQLite's precedence:

| Text | Grouping |
| --- | --- |
| `NOT x NOT NULL` | `NOT (x NOT NULL)` |
| `a = b = c` | `(a = b) = c` |
| `a = b < c` | `a = (b < c)` |
| `-x COLLATE c` | `(-x) COLLATE c` |
| `x BETWEEN a AND b AND c` | `(x BETWEEN a AND b) AND c` |
| `x LIKE a = b` | `(x LIKE a) = b` |

`TestParse_expressions` and `TestParse_statements` in `parser_test.go` pin the tree for every form of expression and every kind of statement. Read them as the reference.

### Spans and tokens

Every node records its `Span`, the bytes `[Start, End)` of the source that its tokens and its children cover. Every token that a node consumes is a field of type `Token` or `[]Token`. Spaces and comments are not tokens of the tree.

The parser sets the span when a node is complete: it starts at the node's first token and ends at the last token consumed. The span is not computed from the tokens afterwards, so the ownership check tests it.

Two structs are not nodes: `QualifiedName`, for a name with an optional schema, and `IfNotExists`. Their tokens belong to the node that holds them.

## The ownership rule

The rule has four parts:

1. Exactly one node owns every token that the parser read for a statement. These are all tokens but spaces and comments, from the statement's first token to its end, its own semicolon included. The empty statements before it are not part of it.
2. The span of a node covers exactly its own tokens and its children.
3. The own tokens and the children of a node do not overlap.
4. A node without tokens has an empty span inside its parent. Only an empty argument of a virtual table is such a node.

The check is generic, so no list of node kinds can go stale:

- `Children` and `Tokens` read the fields of any node through reflection, with a plan cached per type. A field must be a `Token`, a `[]Token`, a node, a slice of nodes, or a struct or pointer to a struct made of those. Any other field type, such as `bool`, `int`, or `string`, panics. A token cannot hide where the walk does not look, and a node cannot keep what was written in anything but tokens.
- `TestPlanOf` in `ast_internal_test.go` parses the package's source with `go/parser`. It checks that every node type is in the test's list and has a valid plan, and that a node with an `int` field panics.
- `ast.CheckOwnership` compares `ast.AllTokens(root)` with the tokens that the tokenizer gives for the statement's span, and checks the spans of every node.
- `TestCheckOwnership` in `check_test.go` proves the check with trees broken on purpose. It finds each one: a token that no node owns, a token that two nodes own, a token that was not read, a span too wide, a span too narrow, a child that overlaps a token of its parent, a token of the wrong kind, and an empty node outside its parent.

The check runs on every statement that parses in the corpus test, the mutant test, the generator test, `TestParse_sqlite`, the shape tests, and both fuzz tests.

## How the parser follows SQLite

Lemon turns `parse.y` into an LALR(1) parser, with operator precedence, `%fallback` of keywords to names, `%wildcard`, and token classes. The parser must match that automaton, not the grammar file alone.

### The token stream

The parser reads tokens lazily, as `sqlite3RunParser` in `tokenize.c` hands them to SQLite's parser:

- It skips spaces and comments.
- A NUL byte ends the text.
- At the end of the text it feeds an empty semicolon, unless the last token was a semicolon, and then the end of input.
- It reports an unrecognized token when the parse reaches it.
- WINDOW, OVER, and FILTER become keywords or names as `analyzeWindowKeyword`, `analyzeOverKeyword`, and `analyzeFilterKeyword` decide. WINDOW is a keyword if a name and AS follow it. OVER is a keyword after `)` and before `(` or a name. FILTER is a keyword after `)` and before `(`.

### Expressions

Expressions are read by precedence climbing, with Lemon's precedences from `parse.y`:

| Precedence | Tokens | Associativity |
| --- | --- | --- |
| 1 | OR | left |
| 2 | AND | left |
| 3 | NOT | right |
| 4 | IS MATCH LIKE_KW BETWEEN IN ISNULL NOTNULL NE EQ | left |
| 5 | GT LE LT GE | left |
| 6 | ESCAPE | right |
| 7 | BITAND BITOR LSHIFT RSHIFT | left |
| 8 | PLUS MINUS | left |
| 9 | STAR SLASH REM | left |
| 10 | CONCAT PTR | left |
| 11 | COLLATE | left |
| 12 | BITNOT, and unary PLUS and MINUS | right |

An operand continues over an operator of higher precedence. It also continues over an operator of equal precedence if that operator is right-associative. Among the tokens that can follow an expression, only NOT is.

- **NOT after an expression** starts NOT NULL, NOT LIKE, NOT BETWEEN, or NOT IN. It has NOT's precedence 3, because the precedence of a rule is that of its leftmost terminal that has one. So `NOT x NOT NULL` is `NOT (x NOT NULL)`.
- **BETWEEN.** The low operand continues over every operator but an AND at its top level, which is BETWEEN's own. The high operand has BETWEEN's precedence: Lemon resolves the reduce-reduce conflict with `expr AND expr` by precedence. So `x BETWEEN a AND b AND c` is `(x BETWEEN a AND b) AND c`.
- **IS NOT.** A NOT after IS belongs to IS, and the right operand has the precedence of IS.

### Keywords as names

With the options of the build, `parse.y` lets 73 keywords fall back to the name ID. Lemon tries the fallback only for a token that has no action of its own in the state. So at each point the parser first checks the keywords that the grammar can use there, and then accepts any fallback keyword as a name.

These keywords stay keywords where a name could also stand:

- CAST, RAISE, CURRENT_DATE, CURRENT_TIME, and CURRENT_TIMESTAMP where an expression starts
- WITH after `(` where a SELECT can start, but not in the arguments of a function
- CURRENT and UNBOUNDED at a frame bound
- PARTITION, RANGE, ROWS, and GROUPS at the start of a window
- RECURSIVE after WITH
- WITHOUT among the table options
- CURRENT_DATE, CURRENT_TIME, and CURRENT_TIMESTAMP after DEFAULT

So `select cast.* from t` fails at the `.`, and `select abort.* from t` parses.

GENERATED is the one keyword that does not follow this order. A patched Lemon report found it. After a column's name or type, GENERATED is a word of the type: `a int generated always as (1)` has the type `int generated always`. After `AS (expr)` it is the name of the storage. In those states, the reductions that the keyword would need are the default reductions. Lemon does not list a default reduction as an action, so GENERATED has no action of its own and falls back to ID.

### Other points of the grammar

- **`%wildcard ANY`.** The arguments of CREATE VIRTUAL TABLE are any tokens with balanced parentheses, the semicolon too. The empty semicolon at the end of the text matches ANY as well, and the end of input after it gives `incomplete input`.
- **ON after a FROM item.** `%nonassoc ON` has a higher precedence than the empty rule `on_using ::= . [OR]`, so an ON after a FROM item always belongs to the join. `insert into t select * from u on conflict do nothing` fails at `do`. An ON after the first item parses too: `select * from t on 1` keeps the ON on that item, and the error is left to the [pass after the parser](#from-join-with-and-multi-row-values).
- **The empty first table option.** `create table t(a), strict` parses.

### Error messages

The parser gives SQLite's three syntax messages:

- `near "X": syntax error`, with the token's raw text and its offset
- `incomplete input`, with the offset -1, on the empty semicolon or the end of input
- `unrecognized token: "X"`, at the token

A syntax error unwinds the parser by a panic with a private value, which `run` recovers. `run` does not recover any other panic.

### `#N` and digit separators

SQLite makes two checks in grammar actions and reports them as syntax errors. The parser makes them itself:

- A variable `#N`, which is a `#` and a digit, gives `near "#N": syntax error`.
- A misplaced digit separator gives `unrecognized token: "..."` with the offset -1. The message shows the number partly rewritten, as `sqlite3DequoteNumber` rewrites it in place: `1__2__3` gives `12_2__3`.

SQLite raises both when the next token arrives. The parser records a pending error with the index of that token:

- If that token is a syntax error, the syntax error wins. `select 1_ 2` gives `near "2": syntax error`, and `select (1_` gives `incomplete input`.
- Otherwise the pending error is the result, even if a later token has a syntax error. SQLite stops after the token on which the error was raised.

The `#N` error has the offset of the variable, with one exception. It has the offset -1 when the next token is the final semicolon of a SELECT, CREATE VIEW, ATTACH, or DETACH. The actions of these commands read the schema of the new connection even after the error. That runs SQL of SQLite's own, which resets the offset. DELETE, INSERT, UPDATE, VACUUM, CREATE INDEX, and CREATE TABLE AS keep the offset: `select #1` gives -1, and `delete from t where #1` gives 20.

The schema is read only once, so the offset stays when SQLite has read the schema earlier in the same statement. That happens for a multi-row VALUES: `sqlite3MultiValues` in insert.c compiles the rows into a co-routine while it parses, and reads the schema first, when no common table expression came before it in the statement, the new row is constant, and the co-routine has started or the row before has no affinity. `values(0),(0) union select #0` keeps the offset 25, and `values(0),(a) union select #0` gives -1. "Constant" is SQLite's `exprIsConst`, which `constant.go` follows on the tree. It depends on whether a function is deterministic, so `functions.go` holds the built-in functions of the binding, as `pragma_function_list` lists them. `TestBuiltinFunctions` checks that table against the binding, and against SQLite's decision for a call of every function with 0 to 4 arguments; `TestParse_schemaRead` compares 1,034 texts of this kind with SQLite.

## Limits

An error of a limit has `Error.Limit` set and a message that starts with `s4ql: `, which no message of SQLite does.

### Depth

`MaxDepth` is 2500. The parser counts one level for:

- each operand of an expression
- a SELECT in parentheses in a FROM clause or in a common table expression
- a join in parentheses

Each level holds at least one entry of the stack of SQLite's parser: the operator or the `(` before it. The depth therefore never exceeds that stack. `TestRuleCoverage` and `TestParse_depth` check this against SQLite's own automaton, on the corpus, on generated sentences, and on 14 kinds of deep nesting.

SQLite's stack holds at most 2,499 entries above the bottom (SQLITE_LIMIT_PARSER_DEPTH is 2500). In SQLite, 2,493 nested parentheses parse and 2,494 give `Recursion limit`. The parser reads `select` with up to 2,499 nested parentheses. It rejects 2,500 with `s4ql: the statement is nested deeper than the limit of 2500`, and SQLite rejects that text too. `Recursion limit` itself is an error for the [pass after the parser](#the-parser-engine).

The limit never rejects what SQLite parses, and it affects no entry of the corpus. A million nested parentheses fail at once.

### Length

`MaxLength` is 1,000,000,000 bytes, SQLite's SQLITE_LIMIT_SQL_LENGTH. The parser counts the tokens it reads, spaces and comments included. The error is `s4ql: the statement is longer than the limit of 1000000000 bytes`. The parser counts from where its parse starts, as SQLite does for each prepare. SQLite also refuses a whole text longer than that before it parses, and the oracle does not say on such texts.

### Memory and time

Memory is proportional to the statement. The parser reads tokens lazily, and the tree is the size of the text.

Time is linear in the length and in the nesting. `BenchmarkParse_long` shows a constant rate in MB/s from 1,000 to 100,000 items, for lists, chains of operators, rows of VALUES, many statements, and nesting.

## Errors left to the pass after the parser

SQLite runs the actions of its grammar while it parses, and some actions raise errors that are not syntax errors. The parser accepts every statement whose only fault is such an error. The tree keeps all that the pass needs to raise the error at the moment SQLite does.

This list is researched from SQLite's source and is partly verified against SQLite. The file and line references are to `src/` of SQLite 3.54.1. In a message, `%s`, `%T`, `%d`, and the like are SQLite's format verbs. The marks are:

- **S**: the error depends on the schema or on the state of the connection.
- **B**: the behaviour changes while SQLite loads a schema (`db->init.busy`).
- **L**: the error is a limit. The value given is its default.

### Timing rules

1. The parse loop stops after the token whose processing raised an error (tokenize.c:717).
2. Of the messages raised while one token is handled, the last wins, because `sqlite3ErrorMsg` replaces the message. `CREATE TRIGGER tr AFTER INSERT ON nosuch.t ...` gives `trigger tr cannot reference objects in database nosuch`, which replaces `no such table`.
3. A syntax error on the same token replaces any action error from that token. `CREATE TABLE x(a) bar )` gives `near ")": syntax error`.
4. The actions of the `cmd ::= ...` rules run on the semicolon after the statement. Their errors are seen only then.
5. Some helpers do nothing once an error is set: `sqlite3CreateIndex`, `sqlite3ExprSetHeightAndFlags`, `sqlite3ExprAddCollateToken`, `triggerStepAllocate`, and `%stack_overflow`.
6. Two errors have a result code other than SQLITE_ERROR. `no such collation sequence` is SQLITE_ERROR_MISSING_COLLSEQ, and the authorizer gives SQLITE_AUTH.

### Raised while the statement is read

SQLite raises these before the statement's final semicolon.

#### The parser engine

| Message | Source | Marks | Raised when |
| --- | --- | --- | --- |
| `Recursion limit` | parse.y:53 | L | `%stack_overflow`: the stack would grow past SQLITE_LIMIT_PARSER_DEPTH (2500). |

#### Token checks with syntax messages

s4ql's parser [makes these two checks itself](#n-and-digit-separators).

| Message | Source | Raised when |
| --- | --- | --- |
| `near "%T": syntax error` | parse.y:1221 | `expr ::= VARIABLE`: a `#` followed by a digit, outside nested parses. |
| `unrecognized token: "%s"` | util.c:356 | `sqlite3DequoteNumber` through `term ::= QNUMBER`: a `_` that is not between two digits (hex digits after `0x`). The message holds the token's text as rewritten in place so far. The last report wins. |

#### The code of parse.y itself

| Message | Source | Marks | Raised when |
| --- | --- | --- | --- |
| `unknown table option: %.*s` | parse.y:240, parse.y:248 | | `table_option ::= WITHOUT nm` with a name that is not `rowid`, and `table_option ::= nm` with a name that is not `strict`. The raw text is compared, so `WITHOUT "rowid"` fails. |
| `%s clause should come after %s not before` | parse.y:556 | | `parserDoubleLinkSelect`: ORDER BY or LIMIT on a part of a compound that is not the last. The second `%s` is the operator after it: UNION, UNION ALL, INTERSECT, or EXCEPT. It is called from `select ::= selectnowith` and `select ::= WITH ...`, from the compound rule when its right side is a multi-row VALUES, and from a vector IN list. |
| `too many terms in compound SELECT` | parse.y:566 | L | More than SQLITE_LIMIT_COMPOUND_SELECT (500) parts, not counting multi-row VALUES. |
| `syntax error after column name "%.*s"` | parse.y:1684 | B: silent | `parserAddExprIdListTerm`, from `eidlist`: a COLLATE or ASC/DESC after a name in the columns of CREATE VIEW, a CTE, FOREIGN KEY (...), or REFERENCES t(...). |
| `the INDEXED BY clause is not allowed on UPDATE or DELETE statements within triggers` | parse.y:1799 | | |
| `the NOT INDEXED clause is not allowed on UPDATE or DELETE statements within triggers` | parse.y:1804 | | |

#### Shared helpers

| Message | Source | Marks | Raised when |
| --- | --- | --- | --- |
| `unknown database %T` | build.c:1054 | S | `sqlite3TwoPartName`: a schema that is not main, temp, or attached. From `sqlite3StartTable` (CREATE TABLE, CREATE VIRTUAL TABLE) and `sqlite3BeginTrigger`. |
| `object name reserved for internal use: %s` | build.c:1117 | S under DEFENSIVE, for shadow tables | `sqlite3CheckObjectName`: a name that starts with `sqlite_`, for a table, a virtual table, or a trigger. |
| `no such table: %s`, `no such table: %s.%s` | build.c:462, build.c:464 | S | `sqlite3LocateTable`, from `sqlite3BeginTrigger` and `sqlite3AlterBeginAddColumn`. |
| The errors of `sqlite3ReadSchema`, such as `malformed database schema (%s)` | prepare.c:490 | S | Depends on the environment. |
| `unable to open a temporary database file for storing temporary tables` | build.c:5418 | | Depends on the environment. |
| `not authorized` | auth.c:230 | | Only with an authorizer. |
| `authorizer malfunction` | auth.c:91 | | Only with an authorizer. |
| `not authorized to use function: %#T` | build.c:716 | | |

#### CREATE TABLE

These come from `create_table`, `columnname`, `ccons`, and `tcons`. After IF NOT EXISTS finds the table, the checks that build the table are silent. The errors of table options, of `eidlist`, and of expressions still fire.

| Message | Source | Marks | Raised when |
| --- | --- | --- | --- |
| `temporary table name must be unqualified` | build.c:1294 | | `sqlite3StartTable`, on the token after the name. |
| `%s %T already exists` | build.c:1345 | S | Not with IF NOT EXISTS. |
| `there is already an index named %s` | build.c:1355 | S | Even with IF NOT EXISTS. |
| `too many columns on %s` | build.c:1564 | L | `sqlite3AddColumn`: more than SQLITE_LIMIT_COLUMN (2000). |
| `duplicate column name: %s` | build.c:1611 | S for ALTER ADD COLUMN | The dequoted name is compared without regard to case. For ALTER ADD COLUMN it is compared with the existing columns. |
| `default value of column [%s] is not constant` | build.c:1803 | | `sqlite3AddDefaultValue`. |
| `cannot use DEFAULT on a generated column` | build.c:1809 | | |
| `error in generated column "%s"` | build.c:2083 | | `sqlite3AddGenerated`: a DEFAULT or an earlier AS on the column, or a name after the expression that is not VIRTUAL or STORED. |
| `generated columns cannot be part of the PRIMARY KEY` | build.c:1872 | | |
| `table "%s" has more than one primary key` | build.c:1909 | | `sqlite3AddPrimaryKey`. |
| `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY` | build.c:1952 | | |
| `unsupported use of NULLS %s` | build.c:3999 | | NULLS FIRST or LAST in a PRIMARY KEY or UNIQUE list. |
| `too many columns in %s` | expr.c:2283, through build.c:4226 | | More than 2000 terms in a UNIQUE or PRIMARY KEY. The `%s` is `index`. |
| The errors of name resolution, listed below | resolve.c, through build.c:4299 | | On the terms of a table PRIMARY KEY(...) or UNIQUE(...), against the table that is being built. |
| `expressions prohibited in PRIMARY KEY and UNIQUE constraints` | build.c:4304 | | |
| `no such collation sequence: %s` | callback.c:230 | S, B: skipped | Through the inline index (build.c:4347), and through `ccons ::= COLLATE ids` (`sqlite3AddCollateType`, build.c:2005). S because collations are registered on the connection. |
| `conflicting ON CONFLICT clauses specified` | build.c:4445 | | |
| `foreign key on %s should reference only one column of table %T` | build.c:3720 | | `sqlite3CreateForeignKey`: a column's REFERENCES with more than one target column. |
| `number of columns in foreign key does not match the number of columns in the referenced table` | build.c:3727 | | |
| `unknown column "%s" in foreign key definition` | build.c:3769 | | |

The errors of name resolution on the terms of a table PRIMARY KEY or UNIQUE are:

- `no such column: %s`, as for `UNIQUE(rowid)`
- `%s: "%s" - should this be a string literal in single-quotes?`
- `%s prohibited in index expressions`, for the "." operator, non-deterministic functions, parameters, and subqueries
- `no such function: %#T` (B: silent)
- `wrong number of arguments to function %#T()`
- `misuse of %s function %#T()`
- `%#T() may not be used as a window function`
- `FILTER may not be used with non-aggregate %#T()`
- `ORDER BY may not be used with non-aggregate %#T()`
- `unsafe use of %#T()`
- `row value misused`
- `second argument to %#T() must be a constant between 0.0 and 1.0`

These errors are S where they depend on the functions that are registered on the connection.

#### ALTER TABLE ... ADD COLUMN

These come from `alter_add`, on the token after the type. `sqlite3AlterBeginAddColumn` (alter.c:509) raises them, and all are S.

| Message | Source |
| --- | --- |
| The `no such table` messages | See [Shared helpers](#shared-helpers). |
| `virtual tables may not be altered` | alter.c:526 |
| `Cannot add a column to a view` | alter.c:533 |
| `table %s may not be altered` | alter.c:40 |

After them, every check of [CREATE TABLE](#create-table) applies to the new column.

#### CREATE VIRTUAL TABLE

These come from `create_vtab`, on the token after the name of the module.

- The errors of `sqlite3StartTable`, `sqlite3TwoPartName`, and `sqlite3CheckObjectName`.
- `too many columns on %s` (vtab.c:369), from `addModuleArgument`, as each argument is appended. That is when the next argument starts.
- A module that does not exist is not an error at prepare time.

#### CREATE TRIGGER

`sqlite3BeginTrigger` raises these from `trigger_decl`, on BEGIN, in this order:

| Message | Source | Marks | Raised when |
| --- | --- | --- | --- |
| `temporary trigger may not have qualified name` | trigger.c:131 | | |
| `unknown database %T` | See [Shared helpers](#shared-helpers). | S | |
| `no such table: %s` | See [Shared helpers](#shared-helpers). | S | The second lookup gives `no such table: main.nosuch`. |
| `%s %T cannot reference objects in database %s` | attach.c:510 | S | A trigger that is not TEMP, on a table of another schema. |
| `cannot create triggers on virtual tables` | trigger.c:186 | S | |
| `cannot create triggers on shadow tables` | trigger.c:190 | | Under DEFENSIVE. |
| `object name reserved for internal use: %s` | See [Shared helpers](#shared-helpers). | | |
| `trigger %T already exists` | trigger.c:208 | S | Not with IF NOT EXISTS. |
| `cannot create trigger on system table` | trigger.c:225 | | |
| `cannot create %s trigger on view: %S` | trigger.c:234 | S | |
| `cannot create INSTEAD OF trigger on table: %S` | trigger.c:239 | S | |

The steps of the trigger raise these from `trigger_cmd`, at each `;` inside the body:

| Message | Source | Raised when |
| --- | --- | --- |
| `qualified table names are not allowed on INSERT, UPDATE, and DELETE statements within triggers` | trigger.c:489 | |
| `unsupported use of NULLS %s` | Through `sqlite3TriggerInsertStep`. | In the target of an upsert. |
| `cannot use RETURNING in a trigger` | build.c:1504 | `sqlite3AddReturning`, on RETURNING in an INSERT step. |
| `too many FROM clause terms, max: %d` | See [FROM, JOIN, WITH, and multi-row VALUES](#from-join-with-and-multi-row-values). | UPDATE ... FROM with more than 200 terms. |

#### FROM, JOIN, WITH, and multi-row VALUES

| Message | Source | Raised when |
| --- | --- | --- |
| `too many FROM clause terms, max: %d` | build.c:4893 | More than 200 terms in one list. SQLITE_MAX_SRCLIST is set at compile time. |
| `a JOIN clause is required before %s` | build.c:5161 | ON or USING on the first item of a list. |
| `unknown join type: %T%s%T%s%T` | select.c:307 | `sqlite3JoinType`: a word that is not NATURAL, LEFT, OUTER, RIGHT, FULL, INNER, or CROSS, or a combination that is not allowed. |
| `duplicate WITH table name: %s` | build.c:5854 | |

Multi-row VALUES is a special case. When every row is constant and the statement has no WITH, SQLite generates code for the rows while it parses (`sqlite3MultiValues`, insert.c:673). It can then raise these errors before the end of the statement. Otherwise they come after the parse.

| Message | Source |
| --- | --- |
| `all VALUES must have the same number of terms` | select.c:3168 |
| `row value misused` | expr.c:5737 |
| `hex literal too big: %s%#T` | expr.c:4449 |
| `too many columns in result set` | select.c:6456 |

#### Expressions

| Message | Source | Marks | Raised when |
| --- | --- | --- | --- |
| `Expression tree is too large (maximum depth %d)` | expr.c:815 | L | The height is above SQLITE_LIMIT_EXPR_DEPTH (1000). |
| `too many arguments on function %T` | expr.c:1208 | L | More than SQLITE_LIMIT_FUNCTION_ARG (1000). |
| `ORDER BY may not be used with non-aggregate %#T()` | expr.c:1223 | | ORDER BY in the arguments together with OVER. |
| `too many terms in ORDER BY clause` | expr.c:1268 | | More than 2000. |
| `DISTINCT is not supported for window functions` | window.c:1323 | | |
| `variable number must be between ?1 and ?%d` | expr.c:1369 | | |
| `too many SQL variables` | expr.c:1403 | L | More than 32766. |
| `IN(...) element has %d term%s - expected %d` | expr.c:1132 | | A row value on the left of IN, and a list of another width on the right. |
| `%d columns assigned %d values` | expr.c:2138 | | SET (a, b) = a value of another width. |

#### Windows

| Message | Source | Raised when |
| --- | --- | --- |
| `unsupported frame specification` | window.c:1224 | |
| `no such window: %s` | window.c:640 | A base window that is not an earlier definition of the same WINDOW clause. |
| `cannot override %s of window: %s` | window.c:1295 | The first `%s` is `PARTITION clause`, `ORDER BY clause`, or `frame specification`. |

### Raised on the semicolon at the end

The actions of the commands run on the final semicolon. They generate code and resolve names, and their errors are not listed one by one. None of them raises a message that looks like a syntax error.

- `sqlite3EndTable`: STRICT types, a missing PRIMARY KEY, the resolution of CHECK and of generated columns, AS SELECT
- `sqlite3CreateView`: `parameters are not allowed in views`, then the errors of `sqlite3StartTable`, which are raised here for a view
- `sqlite3Select`, after `sqlite3ReadSchema`
- `sqlite3Insert`
- `sqlite3Update`: first `too many columns in set list`
- `sqlite3DeleteFrom`
- `sqlite3CreateIndex`
- `sqlite3FinishTrigger`
- `sqlite3DropTable`, `sqlite3DropIndex`, and `sqlite3DropTrigger`
- `sqlite3Pragma`
- `sqlite3Attach` and `sqlite3Detach`
- `sqlite3Vacuum`, `sqlite3Reindex`, and `sqlite3Analyze`
- the `sqlite3Alter*` functions
- `sqlite3VtabFinishParse`

## How it is tested

The tests are in `internal/sql/parser` and `internal/sql/ast`.

Against SQLite on many texts:

- `TestParse_corpus` runs the whole corpus of 139,234 statements. The parser and SQLite agree on 138,926, SQLite does not say on 308, and they disagree on none. It checks ownership on every statement that parses.
- `TestParse_mutants` runs three one-token mutants of every corpus entry, 416,271 texts, with no disagreement. A run with `-args -mutants 40 -seed 1000` compared 5,550,280 mutants with no disagreement.
- `TestGenerate` runs random sentences of the grammar. A generator derives them from the rules of `parse.y`, from the start symbol `input`. Half the time it prefers the least used rule of a nonterminal, and it takes the shortest rules when its budget runs out. Names come from a pool of plain names, every quote style, every fallback keyword, the join keywords, and some keywords that are not names. By default it runs 20,000 sentences and two mutants of each, 60,000 texts, with no disagreement. A run with `-args -generated 400000 -seed 77` compared 1,200,000 texts with no disagreement, of which 415,679 parse.
- `TestParse_sqlite` compares about 200 texts written by hand for corners that the corpus touches little.
- `TestParse_schemaRead` and `TestBuiltinFunctions` compare the offset of `#N` after a multi-row VALUES, for 94 forms of expression in 11 places, and for a call of every built-in function.

Against SQLite's grammar and automaton:

- `TestGrammar` reads `testdata/parse.y` and resolves its `%if` sections for the options of the build. It checks that its 412 rules are exactly those in the comments of the generated parser in the vendored amalgamation, and that its 73 `%fallback` keywords are exactly the parser's. `make corpus` copies `parse.y` unchanged from the verified source archive, and `go run ./internal/sql/corpus/update -check` checks the copy.
- `TestRuleCoverage` in `lalr_test.go` runs SQLite's own LALR automaton over the tokens that the parser read, for every corpus entry and for 20,000 generated sentences. It reads the automaton from the tables in the vendored amalgamation and drives it as `lempar.c` does. It checks that the automaton and the parser agree on acceptance and on the token of every syntax error, and that the parser's depth never exceeds the automaton's stack.

Pinned results:

- `TestParse` pins the result for 34 texts that are easy to get wrong, and checks each against SQLite too.
- `TestParse_expressions` and `TestParse_statements` pin the shape of the tree for every form of expression and every kind of statement.
- `TestParseAll` checks the offsets of several statements in one text, the stop at the first error, and a text without statements.
- `TestParse_depth` and `TestParse_length` check the [limits](#limits).
- `TestPlanOf` and `TestCheckOwnership` in `internal/sql/ast` check the [ownership rule](#the-ownership-rule) and its check.

Under `-short`, the race detector, or the address sanitizer, the corpus tests use every tenth entry and the generator a tenth of its sentences. Rule coverage is then only reported, not required.

### Rule coverage

`TestRuleCoverage` reports which of the 412 rules the statements that parse reduce:

- The automaton reduces 395 rules for the corpus, and 398 with the generator.
- The generator alone reaches three rules: `expr ::= idj LP distinct exprlist ORDER BY sortlist RP filter_over`, `expr ::= expr IS DISTINCT FROM expr`, and `window ::= nm PARTITION BY nexprlist orderby_opt frame_opt`.
- The other 14 are unit rules without code, such as `expr ::= term` and `cmdlist ::= ecmd`. Lemon folds them into gotos, so no automaton reduces them. Each is used in the derivation of a generated sentence that parses.

### Statement coverage

Go's statement coverage of package `parser` is 99.5%. Five places are not covered:

- the default case of `tableConstraint`, because its caller checks the token first
- the code that passes on a panic that is not a syntax error, which only a bug reaches
- the guard in `advance`, which only a bug reaches
- the guard in `Tokens` against an empty token, which the tokenizer does not give inside a statement
- the lookahead past the end of input, because no rule needs three tokens of lookahead there

### Fuzzing

`make fuzz` runs both fuzz tests.

- `FuzzParse` is seeded with every 25th corpus entry and with `own.txt`. It compares the parser with SQLite on the text, and checks the ownership of every statement that `ParseAll` reads from it.
- `FuzzGenerate` fuzzes the seed and the budget of the generator, and one mutant of the sentence.

A run of `FuzzParse` for 40 minutes on 10 cores made 352,479,538 executions and found nothing. A second run found, after 135,493,514 executions in 19 minutes, that a multi-row VALUES can keep the offset of a `#N` error (see [`#N` and digit separators](#n-and-digit-separators)); the input is in `testdata/fuzz/FuzzParse`. After the fix, a third run of 30 minutes made 279,930,645 executions and found nothing, 767,903,697 executions in all. A run of `FuzzGenerate` for 10 minutes made 16,059,438 executions and found nothing.

### Run times

Measured on an Apple M4; the numbers under the address sanitizer are from a Linux container in Docker on the same machine.

- `go test ./internal/sql/parser` takes about 10 s. The corpus test, the mutant test, the generator test, and rule coverage take most of it.
- With `-race` it takes about 10 s, since the tests on the corpus then use a sample.
- Under the address sanitizer it takes about 20 s, on the same sample, and `make test-asan` passes.
- With `-count=20` the package took 385 s.

`BenchmarkParse` parses every statement of the corpus: about 1.4 µs per statement, 50 MB/s, with about 22 allocations per statement.

## Surprises

SQLite behaviour that one would not guess:

- GENERATED is a word of a column's type: `a int generated always as (1)` has the type `int generated always`. See [Keywords as names](#keywords-as-names).
- `create table t(a), strict` parses, because the first table option may be empty.
- The offset of a `#N` error depends on the command, on whether the connection has read its schema, and so on whether a multi-row VALUES before it has constant rows and on which functions are deterministic. See [`#N` and digit separators](#n-and-digit-separators).
- `select 1 end` parses: END is an alias.
- `create table if(a)` fails at the `(`: IF after CREATE TABLE is the keyword.
- `x BETWEEN a OR b AND c` fails. `a OR (b AND c)` is the low operand, and BETWEEN still needs its AND.
- The arguments of a function read WITH as a name, but a `(` in an expression reads it as the start of a SELECT.
- Lemon renumbers the rules of `parse.y`, with those that have code first. `TestGrammar` therefore compares the two lists sorted.

### A fault found in the oracle

The work on the parser found one fault in the oracle. `create view aux.v as select #1` gave a `SyntaxError` with the offset 28, after the oracle had attached `aux`. In the empty database, the action's `unknown database aux` overwrites the `#N` error. The offset of a `#N` error raised on the final semicolon also depends on whether the schema was read.

The fix is in `internal/sql/oracle`. A `#N` verdict found with setup is now `NotSaid`, `Overwritten`, when the parse reached the statement's end. A `#N` inside a trigger step, which needed a table, stays a `SyntaxError`. `oracle_test.go` has the tests, and the oracle's README describes the rule. No corpus entry was affected.
