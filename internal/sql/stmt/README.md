# The pass after the parser

`internal/sql/stmt` takes SQL text and gives one of four answers:

1. **SQLite's syntax error**, as a `*sqlerr.Error` of [`internal/sql/sqlerr`](../sqlerr/sqlerr.go), the one type of SQLite's errors that s4ql gives, which the engine gives too: the message of SQLite's parser, with the result code 1 and the extended result code 1, when the text is a syntax error in SQLite. This holds also when the text is SQL that s4ql does not support. Only the checks before parsing come first: a text longer than SQLite's limit, with a NUL byte, or with bytes that are not UTF-8 is not supported, whatever else it holds. The byte offset is kept as extra information; it is not part of the promise.
2. **"Not supported"**, as a `*stmt.NotSupportedError`, when SQLite's parser accepts the text and the text is not, token for token, a statement of [slice 1](#slice-1), with the parameter of slice 2, the column types and the literals of slice 4, the several columns of slice 5, the NULL of slice 6, the WHERE of slice 7, the UPDATE and DELETE of slice 8, the CREATE INDEX and DROP INDEX of slice 9, the IF NOT EXISTS and IF EXISTS of slice 10, the DEFAULT of slice 11, the value on the left of a comparison of slice 12, the ORDER BY and LIMIT of a select of slice 13, the RETURNING of an insert, an update, and a delete of slice 14, and the scalar subquery in a WHERE of slice 15. The error matches `stmt.ErrNotSupported` with `errors.Is`, its message starts with `s4ql: not supported: `, and it carries no result code of SQLite.
3. **No statement and no error**, for a text that is empty or holds only spaces, comments, and semicolons. SQLite prepares such a text into no statement and no error.
4. **A supported statement**: a `*stmt.CreateTable`, `*stmt.Insert`, `*stmt.Select`, `*stmt.Update`, `*stmt.Delete`, `*stmt.DropTable`, `*stmt.CreateIndex`, or `*stmt.DropIndex`.

The function is `stmt.Check(src) (stmt.Statement, error)`. The rule of the project is in [docs/roadmap.md](../../../docs/roadmap.md): nothing is supported until it is listed. This package is the place where that rule is enforced, and the [definition](#slice-1) below is the precise form of the roadmap's slices 1 and 2, of the column types and the literals of slice 4, of the several columns of slice 5, of the NULL of slice 6, of the WHERE of slice 7, of the UPDATE and DELETE of slice 8, of the CREATE INDEX and DROP INDEX of slice 9, of the IF NOT EXISTS and IF EXISTS of slice 10, of the DEFAULT of slice 11, of the value on the left of a comparison of slice 12, of the ORDER BY and LIMIT of a select of slice 13, of the RETURNING of an insert, an update, and a delete of slice 14, and of the scalar subquery in a WHERE of slice 15, as the pass implements them.

## The typed form

A supported statement is one of eight types. The engine is built on them and never reads the syntax tree.

| Type | Fields | Meaning |
| --- | --- | --- |
| `CreateTable` | `IfNotExists`, `Table`, `Columns []stmt.Column`, `SQL` | Create a table with one or more columns, in the order of the text. Exactly one column is its primary key. A column can have a DEFAULT. The table is STRICT and WITHOUT ROWID. `IfNotExists` reports whether the statement says IF NOT EXISTS. With it, SQLite does nothing and gives no error when a table of the name exists, whatever its columns are, also for a statement with a `ColumnsError()`. When an index has the name, SQLite gives `there is already an index named t1` with the clause as without it. `SQL` is the text that SQLite keeps for the table in `sqlite_schema`: `CREATE TABLE `, and the text from the name of the table to the semicolon, or to the end of the text, with its spaces and comments; the tests compare it with SQLite's. It never holds IF NOT EXISTS, which comes before the name. It holds each DEFAULT as the statement writes it. |
| `Insert` | `Table`, `Columns []stmt.Name`, `Values []stmt.Term`, `Returning stmt.Returning` | Insert one row. `Columns` is the list of columns of the statement, in the order of the text, and nil when the statement has no list: the values are then for the columns of the table, in the order of the table. `Values` holds the values of the row, in the order of the text. `Returning` is the RETURNING clause, with which the statement gives the row that it inserts, with its values as the table stores them. It is the zero `Returning` for a statement without RETURNING. |
| `Select` | `Table`, `Columns []stmt.Name`, `Where []stmt.Condition`, `OrderBy []stmt.OrderColumn`, `Limit stmt.Term`, `Subqueries []stmt.Select` | Read the rows of one table that meet every condition of its WHERE clause, or every row if it has none, in the order of its ORDER BY clause, and at most as many as its LIMIT clause says. `Columns` holds the names of the select list, in the order of the text, each as often as the list names it. It is nil for `*`, which is every column in the order of the table. `Where` holds the conditions of the WHERE clause, in the order of the text, all joined by AND. It is nil for a statement without WHERE. `OrderBy` holds the columns of the ORDER BY clause, in the order of the text, one or more. No two of them have one name, as `Name.Fold` compares names, and a column need not be one that `Columns` names. It is nil for a statement without ORDER BY. `Limit` is the count of the LIMIT clause, the most rows that the select gives: a literal `stmt.Integer` from 0 to the largest integer of 64 bits, or a parameter. It is the zero `Term` for a statement without LIMIT. `Subqueries` holds the scalar subqueries of the WHERE clause, in the order of the text, which the values of its conditions name by their number. It is nil for a statement without a subquery. |
| `Update` | `Table`, `Set []stmt.Assignment`, `Where []stmt.Condition`, `Returning stmt.Returning`, `Subqueries []stmt.Select` | Change the rows of one table that meet every condition of its WHERE clause, or every row if it has none: each gets the values of the assignments. `Set` holds the assignments of the SET clause, in the order of the text, one or more. No two of them have one column, as `Name.Fold` compares names. `Where` is as for `Select`. `Returning` is the RETURNING clause, with which the statement gives each row that it updates, with the values of the row after the update. It is the zero `Returning` for a statement without RETURNING. `Subqueries` is as for `Select`. |
| `Delete` | `Table`, `Where []stmt.Condition`, `Returning stmt.Returning`, `Subqueries []stmt.Select` | Delete the rows of one table that meet every condition of its WHERE clause, or every row if it has none. `Where` is as for `Select`. `Returning` is the RETURNING clause, with which the statement gives each row that it deletes, with the values that the row had. It is the zero `Returning` for a statement without RETURNING. `Subqueries` is as for `Select`. |
| `DropTable` | `IfExists`, `Table` | Drop one table. `IfExists` reports whether the statement says IF EXISTS. With it, SQLite does nothing and gives no error when no table of the name exists, also when an index has the name. |
| `CreateIndex` | `IfNotExists`, `Index`, `Table`, `Columns []stmt.IndexColumn`, `SQL` | Create one index, which is not unique, on one table. `IfNotExists` reports whether the statement says IF NOT EXISTS. With it, SQLite does nothing and gives no error when an index of the name exists, on any table, whatever its columns are, also when the table after ON lacks a column of the statement. The errors that SQLite gives before it looks for the index stay: `no such table` and `there is already a table named i`. `Index` is the name of the index, and `Table` the name of the table after ON. `Columns` holds the columns of the index, in the order of the text, from 1 to 2,000. No two of them have one name, as `Name.Fold` compares names. `SQL` is the text that SQLite keeps for the index in `sqlite_schema`: `CREATE INDEX `, and the text from the name of the index to the semicolon, or to the end of the text, with its spaces and comments; the tests compare it with SQLite's. It never holds IF NOT EXISTS, which comes before the name. |
| `DropIndex` | `IfExists`, `Index` | Drop one index. `IfExists` reports whether the statement says IF EXISTS. With it, SQLite does nothing and gives no error when no index of the name exists, also when a table has the name. |

Every type has `Span`, the bytes of the statement from its first token to the end of its semicolon, or of its last token if it has none.

A `stmt.Column` is a column as CREATE TABLE declares it. It has five fields:

- `Name` is the name of the column.
- `Type` is the type of the column.
- `PrimaryKey` reports whether the column is the primary key of its table.
- `NotNull` reports whether the column says NOT NULL. A column that is not the primary key and does not say it takes NULL. The primary key can say it too, and SQLite records the same table with it and without it: the primary key of a WITHOUT ROWID table never takes NULL.
- `Default` is the DEFAULT of the column, a `stmt.Default`, which is slice 11. It is the zero `Default` for a column that says no DEFAULT.

A `stmt.Default` is the literal after DEFAULT. It has two fields, as a `Term` has them without its `Parameter`:

- `Value` is the value of the literal. It is nil for a column that says no DEFAULT. For DEFAULT NULL it is `stmt.Null{}`, never nil, so the two cannot be taken for each other.
- `Span` is the bytes of the literal, with its sign. Its text is what SQLite keeps as the default of the column, which `pragma_table_xinfo` gives as `dflt_value`.

The offset of the keyword DEFAULT is not kept. It is the last token before the span that is not a space or a comment.

`CreateTable.Key()` gives the index in `Columns` of the primary key. `CreateTable.ColumnsError()` gives the error that SQLite gives for the columns of the statement, or nil; [CREATE TABLE](#create-table) says when it is not nil.

A `stmt.Term` is one value of the row of an insert, the value of an assignment of an update, the value of a condition of a select, an update, or a delete, or the count of the LIMIT of a select. It has four fields:

- `Value` is the value of a literal. It is nil for a parameter and for a subquery. For the literal NULL it is `stmt.Null{}`, never nil.
- `Parameter` is the number of the parameter that gives the value, from 1, or 0 if the value is a literal or a subquery.
- `Subquery` is the number of the scalar subquery that gives the value, from 1, in the `Subqueries` of the statement, or 0 if the value is a literal or a parameter. It is slice 15. Only the value of a condition of the WHERE of a select, an update, or a delete is a subquery, and never the value of a condition of a subquery itself.
- `Span` is the bytes of the value, with its sign, or of the parameter, or of the subquery, from its opening parenthesis to the end of its closing one.

So the `Term` of a NULL has `Value: stmt.Null{}` and `Parameter: 0`, and the `Term` of a parameter has a nil `Value` and a `Parameter` from 1. The two cannot be taken for each other. The `Term` of a subquery has a nil `Value`, the `Parameter` 0, and a `Subquery` from 1.

A `stmt.Type` is the type of the column as the statement declares it: `TypeInt`, `TypeInteger`, `TypeReal`, `TypeText`, or `TypeBlob`. Its `String` is the name that SQLite records: `INT`, `INTEGER`, `REAL`, `TEXT`, or `BLOB`. INT and INTEGER stay two types, as SQLite records them, though they behave the same in a STRICT, WITHOUT ROWID table. The zero value is no type.

A `stmt.Value` is the value of a literal, and one of five types: `stmt.Integer`, an `int64`; `stmt.Real`, a `float64`; `stmt.Text`, a `string`; `stmt.Blob`, a `[]byte`, which is never nil, also when it is empty; and `stmt.Null`, an empty struct, which is the value of the literal NULL. [INSERT](#insert) says which value each literal has.

A `stmt.Assignment` is one assignment of the SET clause of an update. It has two fields:

- `Column` is the name of the column that gets the value.
- `Value` is the value that the column gets, a `stmt.Term`: a literal or a parameter, as a value of an insert is.

A `stmt.Condition` is one condition of a WHERE clause, of a select, an update, or a delete. It has four fields:

- `Column` is the name of the column that the condition tests.
- `Operator` is the operator of the condition, as it compares `Column` with `Value`.
- `Value` is the value that the column is compared with, a `stmt.Term`: a literal or a parameter, as a value of an insert is, or a scalar subquery, on either side of the column, which is slice 15. For `OpIsNull` and `OpIsNotNull` it is the zero `Term`, which is none of the three.
- `ValueFirst` reports whether the text writes the value on the left of the operator and the column on the right, as in `5 < a`, which is slice 12. It is never true for `OpIsNull` and `OpIsNotNull`, whose column is always on the left.

A condition always reads column, operator, value, whichever side of the operator the text writes the column on. For a comparison with its value first, the pass swaps the operator: `5 < a` is the condition `a > 5`, with the `Column` `a`, the `Operator` `OpGreater`, the `Value` 5, and `ValueFirst` true. SQLite gives the two the same rows, for every operator and every type, NULL too. So code that runs a condition reads every condition one way, and code that ignores `ValueFirst` still gives the right rows. With the operator kept as the text writes it, every reader would have to turn it round, and one that forgot would give wrong rows without an error. The order of the text is kept in `ValueFirst`, in `Column.Offset`, and in `Value.Span`.

A `stmt.Operator` is one of eight constants: `OpEqual`, `OpNotEqual`, `OpLess`, `OpLessOrEqual`, `OpGreater`, `OpGreaterOrEqual`, `OpIsNull`, and `OpIsNotNull`. Its `String` is the operator as SQL writes it: `=`, `!=`, `<`, `<=`, `>`, `>=`, `IS NULL`, or `IS NOT NULL`. The zero value is no operator. `=` and `==` are one operator, and `!=` and `<>` are one. SQLite's tokenizer gives each pair one kind of token, `TK_EQ` and `TK_NE`, and nothing that s4ql shows prints the operator: the annotation of a statement keeps the text of the statement itself.

`Operator.Swapped` gives the operator that compares as the operator does with its two sides swapped: `>` for `<`, `<` for `>`, `>=` for `<=`, `<=` for `>=`, and `=` and `!=` themselves. For a test for NULL, which has one side, and for no operator, it gives no operator. For a condition that says `ValueFirst`, `Operator.Swapped()` is the operator as the text writes it.

A `stmt.IndexColumn` is one column of a create index. It has two fields:

- `Name` is the name of the column.
- `Descending` reports whether the column says DESC. It is false for a column that says ASC and for a column that says neither, which SQLite records as the same index.

A `stmt.OrderColumn` is one column of the ORDER BY of a select, which is slice 13. It has two fields, as an `IndexColumn` has them:

- `Name` is the name of the column.
- `Descending` reports whether the column says DESC. It is false for a column that says ASC and for a column that says neither, which are both in ascending order.

The rows of a select with ORDER BY come in the order of the first column of `OrderBy`. Rows that tie there come in the order of the second, and so on. Each column orders its values as SQLite orders them: NULL comes before every other value in ascending order, and after every other in descending order. The order of rows that tie on every column is not specified.

`Select.Limit` is a `Term` because a `Term` is a literal or a parameter with its span, which is what the count of a LIMIT is. A select without LIMIT has the zero `Term`, which is neither a literal nor a parameter, as the `Value` of a test for NULL is. `Select.Limited()` reports whether the statement has a LIMIT, so `limit 0`, which gives no row, is not taken for no LIMIT. `Limit` is no pointer: no statement holds one, and the statements are compared and printed by value. A `?` of LIMIT comes after those of WHERE in the text, so it has the number after theirs: in `select v1 from t1 where v2 > ? order by v1 limit ?`, the `?` of WHERE has the number 1 and that of LIMIT the number 2. `Select.Parameters()` counts both.

The offsets of the keywords ORDER, BY, and LIMIT are not kept. ORDER is the last token of its kind before the first column of `OrderBy`, and LIMIT the last of its kind before the span of `Limit`.

A `stmt.Returning` is the RETURNING clause of an insert, an update, or a delete, which is slice 14. With it, the statement gives a row for each row that it inserts, updates, or deletes, with the columns that the clause names, as a select gives the columns of its select list. It has two fields:

- `Span` is the bytes of the clause, from the keyword RETURNING to the end of its last column, or of its `*`.
- `Columns` holds the names of the clause, in the order of the text, each as often as the clause names it, as `Select.Columns` holds the select list. It is nil for `*`, which is every column in the order of the table.

An insert and an update give the values of the row after the change, and a delete the values that the row had. The rows come in no order that the statement says.

A statement without RETURNING has the zero `Returning`, which has the zero `Span` and nil `Columns`. `Returning.Present()` reports whether the statement has the clause, so `returning *`, whose `Columns` is nil too, is not taken for no RETURNING. `Returning` is no pointer, as `Select.Limit` is none: no statement holds one, and the statements are compared and printed by value. The span is kept, where the offsets of ORDER BY and LIMIT are not, because `*` has no name with an offset: the offset of the keyword RETURNING is `Span.Start`. RETURNING holds no parameter, so it adds nothing to `Parameters()` of an insert, an update, or a delete.

`Subqueries`, of a `Select`, an `Update`, and a `Delete`, holds the scalar subqueries of the WHERE clause, in the order of the text, which is slice 15. The value of a condition names one by its `Term.Subquery`: a condition whose `Value.Subquery` is k compares its column with the value of the k-th subquery, `Subqueries[k-1]`. That value is the value of the one column of the subquery in the first row that the subquery gives, or NULL if it gives no row. A statement without a subquery has nil `Subqueries`.

A subquery is a `stmt.Select`, of the same type as a select statement:

- `Span` is the bytes of the select, from its SELECT to the end of its last token, without the parentheses. The `Span` of the `Term` that names it holds the parentheses.
- `Table` is its one table.
- `Columns` holds its one result column, the name of a column, or is nil for `*`.
- `Where`, `OrderBy`, and `Limit` are as for a select statement. No condition of a subquery has a subquery as its value, so its `Subqueries` is nil.
- Every name has its offset in the text of the statement, as every name has.

The parameters of a subquery are numbered with those of the statement, in the order of the text, as SQLite numbers them. In `update goqite set timeout = ? where queue = ? and id = (select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1) returning id, body, received`, the `?` of SET has the number 1, that of `queue = ?` the number 2, and the three of the subquery the numbers 3, 4, and 5. A `?` of the LIMIT of a select comes after those of its WHERE and of the subqueries in it. `Parameters()` of a statement counts those of its subqueries too, and `Parameters()` of a subquery counts its own.

The subqueries are a list of the statement, and a `Term` names one by a number, as it names an argument by `Parameter`. A `Term` holds no pointer to a select: no statement holds a pointer, and the statements are compared and printed by value. A select inside a select needs a pointer or a list, so it is a list.

Whether a name of a subquery is a column of its own table or of the table of the statement depends on the tables, so the typed form does not say; see [Subqueries](#subqueries).

A `Name` has three fields, because SQLite prints names in different forms in its messages:

- `Raw` is the name as written, with its quotes. SQLite prints it in `table "t1" already exists`: `create table "t1" …` twice gives `table "t1" already exists`, and `create table T1 …` after `t1` gives `table T1 already exists`.
- `Value` is the name without its quotes, with `""` read as `"`. SQLite prints it in `no such table: t2`, in `table t1 has no column named v2`, and in `no such column: v2`: `insert into "t""1" ("V""2") values (1)` gives `table t"1 has no column named V"2`. The names that CREATE TABLE stores are its `Value`s, and SQLite prints them in `UNIQUE constraint failed: t1.v1`: after `create table Foo (Bar int primary key) …`, `insert into FOO (BAR) values (1)` twice gives `UNIQUE constraint failed: Foo.Bar`.
- `Offset` is the byte offset of the name's token. SQLite gives the offset of the name with `already exists`.

`Name.Fold` gives the value with the ASCII letters in lower case. SQLite takes two names for the same name when their folds are equal. It folds no other letter: `ÆØÅ` and `æøå` are two tables. This holds for the names of columns too: the pass compares the columns of a CREATE TABLE, of the list of an INSERT, of the SET of an UPDATE, and of a CREATE INDEX by their folds.

Each check of the meaning in the tests runs the error of each name against SQLite; see [How it is tested](#how-it-is-tested).

## How it works

`Check` makes its checks in this order:

1. **Before parsing**, it rejects text that is longer than SQLite's limit of 1,000,000,000 bytes (`SQLITE_LIMIT_SQL_LENGTH`), which SQLite refuses with `SQLITE_TOOBIG` before it parses, whatever the text holds. Then it rejects text that has a NUL byte or a byte that is not valid UTF-8, at the first such byte. Valid UTF-8 is as Go's `unicode/utf8` defines it: surrogates are not valid. These checks win over a syntax error.
2. It **parses** the first statement with `parser.Parse`, as `sqlite3_prepare_v2` does. A syntax error is SQLite's. An error of a limit of the parser, which SQLite rejects too with an error that is not a syntax error, is not supported.
3. A text without a statement gives **no statement**.
4. The **recognizer** reads the tree of the statement, and builds the typed form or says why the statement is not supported.
5. If the statement is supported, any text after it must be spaces, comments, and semicolons. Otherwise the text is not supported, at the first token of the **second statement**. The second statement is not parsed: SQLite's prepare of the first statement succeeds whatever follows it, so a later syntax error is not the first statement's. When the first statement is a syntax error or not supported, that answer wins.

### The recognizer

The recognizer is an allow-list over the tokens of the statement. It holds the tokens that the parser read for the statement, from the tokenizer (`parser.Tokens`), in the order of the text, and a cursor on the next one. It walks the tree in the order of the text, and takes each token that the supported form expects in that place: a token it takes must be the token at the cursor. It builds the typed form only from the tokens it took.

So a token that the recognizer does not know stops it, without a check of its own:

- An optional keyword, such as TEMP, or an optional clause, such as GROUP BY, is a field of the tree that the recognizer does not read. Its token is at the cursor when the recognizer takes the next token it expects, and the take fails.
- A node of a type that the recognizer does not know is not entered, so none of its tokens is taken.
- After the statement, every token of the tree must be taken. This check is generic: it holds for any kind of command and any node, also one that the tree gains later.

By the parser's ownership rule, which its tests check on every statement, these are exactly the tokens of `ast.AllTokens` of the tree. The gate does not depend on that rule: the tokens come from the text, so a token of the text that no node holds is never taken, and a token that a node holds and the text does not have cannot be taken.

## Slice 1

A statement is supported if it is one of the eight forms below, token for token. Slice 2 of the roadmap adds one form of the value of INSERT, a parameter, and this section defines it under [INSERT](#insert). Slice 4 adds the column types other than INT and the literals other than an integer, and this section defines them under [CREATE TABLE](#create-table) and [INSERT](#insert). The rest of slice 4, the arguments and the check of a value against the type of its column, is for the engine and the driver. Slice 5 adds a table of several columns, an insert of several values with or without a list of columns, and a select of named columns, and this section defines them under [CREATE TABLE](#create-table), [INSERT](#insert), and [SELECT](#select). What such a statement means for a table depends on the database, so it is for the engine: whether the columns exist, and whether the values are as many as the columns. Slice 6 adds a column that takes NULL and the literal NULL as a value of an insert, and this section defines them under [CREATE TABLE](#create-table) and [INSERT](#insert). Whether a column takes the NULL that an insert gives it depends on the table, so it is for the engine too. The rest of slice 6, the nil argument and the NULL that is stored and read, is for the engine and the driver, which run the forms of slice 6 since 2026-11-20. Slice 7 adds a WHERE of conditions to a select, and this section defines it under [SELECT](#select). Whether the table has the column of a condition, and whether a value has the type of its column, depend on the table, so they are for the engine too. The rest of slice 7, the rows that the conditions choose, is for the engine and the driver, which run the forms of slice 7 since 2026-11-20. Slice 8 adds an update and a delete, each with or without the WHERE of slice 7, and this section defines them under [UPDATE](#update) and [DELETE](#delete). Whether the table has the columns of the assignments and of the conditions, and whether a value has the type of its column, depend on the table, so they are for the engine too. The rest of slice 8, the rows that change and their number, is for the engine and the driver, which run the forms of slice 8 since 2026-11-20. Slice 9 adds a create index and a drop index, and this section defines them under [CREATE INDEX](#create-index) and [DROP INDEX](#drop-index). Whether the table and its columns exist, and whether a name is taken, depend on the database, so they are for the engine too. The rest of slice 9, the index that the statements create and drop, is for the engine and the driver, which run the forms of slice 9 since 2026-11-20. Slice 10 adds IF NOT EXISTS to a create table and to a create index, and IF EXISTS to a drop table and to a drop index, and this section defines them under [CREATE TABLE](#create-table), [DROP TABLE](#drop-table), [CREATE INDEX](#create-index), and [DROP INDEX](#drop-index). Whether the table or the index exists depends on the database, so it is for the engine too. The rest of slice 10, the statement that does nothing where the plain statement gives an error, is for the engine and the driver, which run the forms of slice 10 since 2026-11-21. Slice 11 adds a DEFAULT with a literal to a column of a create table, and this section defines it under [CREATE TABLE](#create-table). Whether a column that an insert leaves out has a DEFAULT depends on the table, so it is for the engine too. The rest of slice 11, the default that an insert stores, is for the engine and the driver, which run the forms of slice 11 since 2026-11-21. Slice 12 adds a comparison with its value on the left to a WHERE, and this section defines it under [SELECT](#select). The rest of slice 12, the rows that such a comparison chooses, is for the engine and the driver, which run the forms of slice 12 since 2026-11-21. Slice 13 adds an ORDER BY and a LIMIT to a select, and this section defines them under [SELECT](#select). Whether the table has a column of an ORDER BY depends on the table, so it is for the engine too. The rest of slice 13, the order of the rows and their number, is for the engine and the driver, which run the forms of slice 13 since 2026-11-21. Slice 14 adds a RETURNING to an insert, an update, and a delete, and this section defines it under [RETURNING](#returning). Whether the table has a column of a RETURNING depends on the table, so it is for the engine too. The rest of slice 14, the rows that such a statement gives, is for the engine and the driver, which run the forms of slice 14 since 2026-11-23. Slice 15 adds a scalar subquery, as the value of a comparison, to the WHERE of a select, an update, and a delete, and this section defines it under [Subqueries](#subqueries). Whether the table of a subquery and its columns exist, and whether a name of a subquery is a column of its own table or of the table of the statement, depend on the database, so they are for the engine too. The rest of slice 15, the value that a subquery gives, is for the engine and the driver. Until the engine runs it, the engine refuses a select, an update, or a delete with a subquery as not supported, with the name that the pass gave it before slice 15, `subquery`, at the opening parenthesis of its first subquery (`engine.subqueryRefusal`), and the driver refuses it with that error when it prepares it. The tables give the tokens in order. A gap between two tokens may hold any spaces and comments. A keyword may be in any case of its letters.

The roadmap's restrictions and these slices define the forms. Where the roadmap leaves the form open, the rule is: when in doubt, it is not supported. A form is supported only if a test against SQLite shows that SQLite treats it exactly as the plain form, or the roadmap names it.

### CREATE TABLE

```
CREATE TABLE [IF NOT EXISTS] table ( column-def [, column-def]... ) STRICT , WITHOUT ROWID
CREATE TABLE [IF NOT EXISTS] table ( column-def [, column-def]... ) WITHOUT ROWID , STRICT
```

A `column-def` is one of these:

```
column type
column type PRIMARY KEY
column type NOT NULL
column type PRIMARY KEY NOT NULL
column type NOT NULL PRIMARY KEY
```

Each of the five can also say `DEFAULT literal`, which is slice 11, at most once, anywhere after the type: before, between, or after PRIMARY KEY and NOT NULL.

- `type` is one bare word, `int`, `integer`, `real`, `text`, or `blob`, in any case of its letters.
- `int` and `integer` are two types. In a STRICT, WITHOUT ROWID table, `integer primary key` is not an alias of the rowid, since there is none, and it behaves as `int primary key` does. But SQLite records the type as `INTEGER`, not `INT`, so it is not the same table, and `Column.Type` keeps the two apart.
- Exactly one column says PRIMARY KEY. It can be at any position.
- Every other column says NOT NULL or does not. A column that does not say it takes NULL, which is slice 6: `pragma_table_xinfo` gives `notnull` 0 for it, and `Column.NotNull` is false.
- The primary key can say NOT NULL too, before or after PRIMARY KEY. SQLite records the same table with it and without it: `pragma_table_xinfo` gives `notnull` 1 for the primary key of a WITHOUT ROWID table either way, and the primary key never takes NULL. `Column.NotNull` keeps what the statement says.
- Both orders of the options are supported. SQLite records the same table for both: `pragma_table_list` gives `wr` 1 and `strict` 1, and `pragma_table_xinfo` the same columns, for `strict, without rowid` and for `without rowid, strict`. The tests compare the schema that each statement makes with that of the plain form, as SQLite's PRAGMAs record it: the tables and their options, columns, and indexes. The text that `sqlite_schema` keeps is not compared, since SQLite keeps it as written.
- The roadmap's restrictions say that all tables are STRICT and WITHOUT ROWID, and [research/sql-front-end.md](../../../docs/research/sql-front-end.md) §3.3 lists a table without the options as rejected. The statement must therefore say both: without them, SQLite makes a table that is not STRICT or has a rowid, and the roadmap's principle is to reject, never to reinterpret.

Two faults of the columns are supported by the pass, and SQLite's error for them is left to the engine:

- A column that has the name of an earlier column, as `Name.Fold` compares names.
- More than 2,000 columns, which is SQLite's limit, `SQLITE_MAX_COLUMN`.

SQLite raises both errors in `sqlite3AddColumn`, while it parses. But `sqlite3StartTable` has looked for the table before that. On a database that has a table of the name, SQLite gives `table t already exists`, and never meets the fault of the columns. So SQLite's answer depends on the database, and the spec accepts a difference from SQLite only for a syntax error. The pass therefore supports the statement, and `CreateTable.ColumnsError()` gives the error that SQLite gives on a database without the table, with the result code 1 and the extended result code 1:

- `duplicate column name: v1`, for the first column whose name an earlier column has. The name in the message is the value of the name of the later column.
- `too many columns on t1`, for the 2,001st column, also when it has the name of an earlier column. The name in the message is the value of the name of the table.

SQLite adds the columns in order, so the first fault in the order of the text decides the error. The engine gives the error after its check of the table. A statement that is not supported for another reason stays not supported, also when SQLite would first give the error of a column named twice: `create table t (a int primary key, a int primary key) strict, without rowid` is `second PRIMARY KEY`, where SQLite gives `duplicate column name: a`, as an unknown type is not supported where SQLite gives `unknown datatype`. The offset of both errors is -1, as SQLite gives it, since neither names a token of the text. The tests check `ColumnsError()` against SQLite on a database with the table and on a database without it, the offset too.

IF NOT EXISTS, which is slice 10, comes between TABLE and the name of the table, and `CreateTable.IfNotExists` is true for it:

- The clause is the three keywords IF, NOT, and EXISTS, each in any case of its letters, with any spaces and comments between them.
- `CreateTable.SQL` is the same with the clause and without it. SQLite keeps the text from the name on, so it keeps `create table If Not Exists t (v int primary key) strict, without rowid` as `CREATE TABLE t (v int primary key) strict, without rowid`.
- After the clause, the name follows the rules of the plain statement, and 88 keywords are names there, as without the clause. `TestCheck` checks each against SQLite.
- The bare word IF as the name after the clause, in any case of its letters, is not supported, as `bare name IF after IF NOT EXISTS`, at the name. SQLite's parser accepts `create table if not exists if (v int primary key) strict, without rowid`. SQLite then keeps `CREATE TABLE if (v int ...`, which it cannot parse again: it gives `malformed database schema (if) - near "(": syntax error` and creates nothing. `"if"` in double quotes is a name like any other.
- Every form that is not supported without the clause is not supported with it, by the same name and at the same token: `create temp table if not exists t ...` is `TEMP` at the TEMP, and `create table if not exists main.t ...` is `schema name` at the `main`. This holds also where SQLite would do nothing because the table exists, as for a second PRIMARY KEY or a type that is not one of the five: the pass sees only the text.

The pass does not know the database, so these facts of SQLite are for the engine. `checkCreateTable` and a subtest of `TestCheck` check them against SQLite; see [How it is tested](#how-it-is-tested).

- With a table of the name, in any case of the ASCII letters, the statement does nothing and gives no error, whatever the columns of that table are. SQLite stops reading the declaration once it has found the table (`pParse->pNewTable` stays null), so it gives no error of the columns either: a statement with a `ColumnsError()` does nothing too.
- Without the table, the statement does what the plain statement does, and the error of `ColumnsError()` comes as for the plain statement.
- The clause silences `table t already exists` and no other error. With an index of the name, SQLite gives `there is already an index named t1`, with the value of the name as the statement writes it.
- A statement that does nothing writes nothing. It leaves SQLite's count of changes as it was, as a statement that creates the table does.
- In a read-only transaction, which the tests run under `pragma query_only`, a statement that does nothing succeeds. A statement that would create the table gives `attempt to write a readonly database`, after the errors of the name and of the columns, as the plain statement does: `create table if not exists u (a int primary key, a int) strict, without rowid` gives `duplicate column name: a` there.

Syntax errors of the clause, which the pass gives as SQLite gives them:

- The clause before TABLE, `create if not exists table t ...`, which is `near "if": syntax error`.
- IF EXISTS in place of IF NOT EXISTS, which is `near "exists": syntax error`.
- IF NOT or IF alone before the name, `create table if not t ...` and `create table if t ...`, which are `near "t": syntax error`.
- The clause twice, which is `near "not": syntax error`.
- A table named `not` or `exists` after the clause, which is a syntax error near that word, and no name after the clause, which is `near "(": syntax error`.
- A table named `if` without the clause, `create table if (...)`, which is `near "(": syntax error`: IF starts the clause there.

DEFAULT, which is slice 11, comes after the type of a column, with a literal after it, and `Column.Default` holds the literal:

- The keyword is in any case of its letters, with any spaces and comments between it and the literal.
- The literal is a literal as [INSERT](#insert) defines it, of each of the five kinds, NULL too, with the same sign. The pass reads it with the code that reads the value of an insert, and gives it the value that SQLite gives its text: `default - /* c */ 1` is -1, `default -0.0` is negative zero, `default 00012` is 12, and `default 1e-400` is `0.0`.
- The literal has the type of its column: an integer for INT and INTEGER, a real for REAL, a string for TEXT, and a blob for BLOB. NULL goes with every type.
- DEFAULT NULL is supported also on a column that says NOT NULL and on the primary key, as SQLite takes both: `v int not null default null` and `k int primary key default null`.
- The primary key can have a DEFAULT, as every other column can.
- A column says DEFAULT at most once, before, between, or after PRIMARY KEY and NOT NULL. SQLite records the same table for every order.
- `Default.Span` is the text that SQLite keeps as the default of the column, from the sign of the literal to its last token, with the spaces and comments between them. `pragma_table_xinfo` gives it as `dflt_value`: `- /* c */ 1` for `default - /* c */ 1`, `'it''s'` for `default 'it''s'`, and `X'aBcD'` for `default X'aBcD'`. A comment between DEFAULT and the literal is not part of it.
- `CreateTable.SQL` follows the same rule as without a DEFAULT. SQLite keeps the text from the name on, with each DEFAULT as written.

The pass does not know the table of an insert, so these facts of SQLite are for the engine. `checkDefaults` and a subtest of `TestCheck` check them against SQLite; see [How it is tested](#how-it-is-tested).

- An insert with a list of columns that leaves out a column with a DEFAULT stores the value of the DEFAULT, of each of the five kinds. A real zero reads back as `0.0`, also for `default -0.0`, as after an insert of `-0.0`.
- A NULL that the insert gives is not replaced by the default. For `a int not null default 0`, `insert into t (k, a, c) values (1, null, 2.5)` gives `NOT NULL constraint failed: t.a`. For `b text default 'x'`, a NULL as the value of `b` is stored.
- DEFAULT NULL on a column that says NOT NULL, or on the primary key, changes nothing. An insert that leaves the column out gives `NOT NULL constraint failed: t.c`, as for the column without the DEFAULT.
- An insert that leaves out a primary key with a DEFAULT stores the default as the key. When a row has that key, it gives `UNIQUE constraint failed: u.k`.
- An insert without a list of columns gives every column a value, so the defaults change nothing: `table t has 5 columns but 1 values were supplied` stays. So does `table t has no column named nosuch` for a list.
- The row that SQLite stores holds the defaults of the columns that the insert leaves out, so it can be too big because of them. With the length limit at 1,000 bytes, a default of 500 bytes and a given value of 600 bytes give `string or blob too big`. NOT NULL comes before that error, also for a column with DEFAULT NULL that the insert leaves out.
- In a read-only transaction, which the tests run under `pragma query_only`, `attempt to write a readonly database` comes before every error of a row: NOT NULL, the type of a value, UNIQUE, and `string or blob too big`. The errors of the columns and of the number of values come before it, as without a DEFAULT.
- An update and a select do not meet the defaults. `set b = null` stores NULL in a column with `default 'x'`, and `set a = null` of `a int not null default 0` gives `NOT NULL constraint failed: t.a`.

Syntax errors of DEFAULT, which the pass gives as SQLite gives them:

- DEFAULT without a literal, `v int default)`, and empty parentheses, `default ()`, which are `near ")": syntax error`.
- A `?` after DEFAULT, also after a sign, `default ?` and `default -?`, which are `near "?": syntax error`.
- A second sign, `default - -1` and `default +-1`, which are `near "-": syntax error`, and `default ~1`, which is `near "~": syntax error`.
- A sign before parentheses, `default -(1)`, which is `near "(": syntax error`.
- Two literals, `default 'x' 'y'`, which is `near "'y'": syntax error`.
- A keyword that is no name after DEFAULT: `default on`, `default not null`, and `default primary key`, which are syntax errors near `on`, `not`, and `primary`.
- A literal that the tokenizer does not read, `default 1.5e` and `default x'0'`, which are `unrecognized token: "1.5e"` and `unrecognized token: "x'0'"`.

So `second sign`, `~ operator`, `NOT operator`, and `parameter after a sign` never name a DEFAULT: the forms that they name in an insert are syntax errors here.

Not supported as a DEFAULT, with what SQLite does where it bears on the decision:

- A literal of another type than its column, and not NULL, as `DEFAULT of another type than its column`, at the literal, its sign included: `v real default 1`, `v int default 1.0`, and `v text default x'31'`. SQLite takes such a declaration in a STRICT table, whatever the two types are, and decides when an insert stores the default. An INT column converts `1.0` and `'1'` to the integer 1, and refuses `1.5` with `cannot store REAL value in INT column t.v`. A TEXT column stores `1.0` as the text `'1.0'`. A BLOB column refuses every value but a blob. By the exact types of slice 4, s4ql refuses a value of another type than its column at every insert. It could never store such a default, so the pass refuses the declaration.
- Every form of a literal that INSERT does not support and that SQLite's grammar takes after DEFAULT, by the name that it has there and at the same token: `default +1` is `+ sign`, `default +null` and `default -null` are `NULL after a sign`, `default 0x10` is `hex literal`, `default 1_000` is `number with a digit separator`, `default 9223372036854775808` is `integer outside 64 bits`, `default 1e999` is `infinite real number`, `default -'a'` is `string after a sign`, `default -x'01'` is `blob after a sign`, and `default current_timestamp` is `current date or time`, also after a `-`. SQLite takes each of them.
- The bare word TRUE or FALSE, as `TRUE or FALSE`. SQLite reads them as 1 and 0. `"true"` in double quotes is `name after DEFAULT`.
- A name after DEFAULT, as `name after DEFAULT`, at the name: a bare word, a keyword that SQLite reads as a name there, such as `abort`, or a name in double quotes, brackets, or backticks. SQLite reads it as a string: `default abc` and `default "abc"` store `'abc'`. The pinned build of SQLite has `DQS=0`, and after DEFAULT it reads a name in double quotes as a string all the same.
- A DEFAULT in parentheses, as `DEFAULT of an expression`, at the parenthesis: `default (1)`, `default (1+1)`, `default (null)`, and `default (?)`. SQLite takes the first three, and leaves the outer parentheses out of `dflt_value`. For `default (?)` it gives `default value of column [v] is not constant` while it parses.
- A second DEFAULT of a column, as `second DEFAULT`, at the later DEFAULT, also when both have one value, and also with NOT NULL between the two. SQLite keeps the last: `default 1 default 2` has the `dflt_value` `2`.
- Another constraint after the literal, by its own name: `default 'a' collate binary` is `COLLATE`, `default null null` is `NULL constraint`, `default 1 unique` is `UNIQUE`, and `default 1 check (v > 0)` is `CHECK`.

What SQLite converts, stores, or keeps for the forms of this list was measured with SQLite's shell only, and the tests of the pass do not check it. `insert ... default values` stays not supported, as `DEFAULT VALUES`; see [INSERT](#insert).

Not supported, with what SQLite does where it bears on the decision:

- A type that is not one of the five bare words. Its name is `column type other than INT, INTEGER, REAL, TEXT, or BLOB`:
  - A type in double quotes, single quotes, or brackets. SQLite dequotes `"text"` and records `TEXT`, but double quotes mean names only.
  - A type of more than one word, such as `int unsigned`, or `int generated always`, which SQLite reads as INT, and a type with a size, such as `text(10)`.
  - `ANY`, which the roadmap's restrictions rule out.
  - Every other type name, such as `varchar` or `double`. SQLite itself refuses an unknown type in a STRICT table, with `unknown datatype for t.v: "foo"`. It does so in `sqlite3EndTable`, after it has parsed the statement, so the oracle says that the text parses. The error is not a syntax error, so by the pass's rule the answer is "not supported", as for `int(10)`. This is not a gap.
- No type.
- A second PRIMARY KEY, on another column or twice on one column, as `second PRIMARY KEY`. SQLite gives `table "t" has more than one primary key` while it parses.
- No PRIMARY KEY, as `table without PRIMARY KEY`, at the `)` after the columns. This holds whatever the columns say, also when they take NULL. SQLite gives `PRIMARY KEY missing on table t` after it has parsed the statement. The error is not a syntax error, so by the pass's rule the answer is "not supported", as for an unknown type.
- The column constraint NULL without NOT before it, as in `v2 text null`, as `NULL constraint`, at the NULL. SQLite reads it as no constraint, so the column takes NULL with it as without it. The roadmap's slice 6 lists only a column without NOT NULL, so the pass rejects the constraint. This holds also before or after NOT NULL, with ON CONFLICT, and on the primary key.
- NOT NULL twice, NOT NULL with ON CONFLICT, and every other column constraint, each with the name of its node, such as `NULL or NOT NULL constraint` and `UNIQUE`. Also `CONSTRAINT name`, ASC or DESC, ON CONFLICT, and AUTOINCREMENT.
- A table constraint, such as `primary key (a, b)` or `unique (b)`, as `table constraint`, at the comma after the last column.
- `STRICT` or `WITHOUT ROWID` alone, none, either twice, a third option, an empty first option (`) , strict, without rowid`), and `"strict"` or `without "rowid"`, which SQLite rejects with `unknown table option`.
- TEMP, a schema name, and `AS SELECT`.

### INSERT

```
INSERT INTO table VALUES ( value [, value]... ) [RETURNING returning-list]
INSERT INTO table ( column [, column]... ) VALUES ( value [, value]... ) [RETURNING returning-list]
```

Without a list of columns, the values are for the columns of the table, in the order of the table. With a list, the columns can be in any order. RETURNING, which is slice 14, comes last, and [RETURNING](#returning) defines it and its `returning-list`.

Each value is a literal or a parameter. A literal is one of five kinds:

- **An integer:** a decimal integer literal that fits a signed 64-bit integer, with at most one `-` in front, and any spaces and comments between the sign and the digits. `-9223372036854775808` is in. Leading zeros are allowed: they are part of a decimal integer literal, and SQLite stores `00012` as 12 and `-0` as 0. SQLite stores every value of this form as the integer it names, with any number of leading zeros, which the tests check against SQLite for each value.
- **A real:** a real literal in every form that SQLite's tokenizer reads as one, such as `1.5`, `.5`, `1.`, `1e3`, `1E+3`, and `1.5e-3`, with any number of digits, with at most one `-` in front, and any spaces and comments after the sign. Its value is the 64-bit float that SQLite gives it, bit for bit. `-0.0` is negative zero. A literal below the smallest float, such as `1e-400`, is `0.0`, as in SQLite.
- **A string** in single quotes, with `''` for a quote in it. Its value is the bytes between the quotes, with `''` read as `'`. It is valid UTF-8 without a NUL byte, since the text is.
- **A blob:** `x'6162'` or `X'6162'`, with hex digits in either case. Its value is the bytes that the digits give. `x''` is the empty blob. An odd number of digits, or a digit that is not hex, is a syntax error in SQLite's tokenizer, `unrecognized token: "x'6'"`, which the pass gives.
- **NULL:** the bare word `null`, in any case of its letters, without a sign. It is slice 6. Its value is `stmt.Null{}`.

The pass reads a real literal with a port, in `real.go`, of `sqlite3AtoF` and `sqlite3Fp10Convert2` of SQLite 3.54.1, with the same integer arithmetic. It does not use Go's `strconv.ParseFloat`, which rounds every text to the nearest float. SQLite keeps only about the first 19 significant digits of a literal, so for a longer one it can round to another float than the nearest. SQLite's own example is `3500000000000000.2500001`: it is `3500000000000000.0` in SQLite, where `strconv.ParseFloat` gives `3500000000000000.5`, and the pass gives SQLite's. A new version of SQLite that changes these routines needs the port changed with it, and `TestCheck_reals` would show it.

A literal that SQLite reads as an infinity, such as `1e999` or `-1e999`, is not supported, as `infinite real number`. Markus decided that s4ql stores no infinity, and the roadmap's slice 4 rejects one.

The pass does not know the table, so `insert into t1 (v1) values ('a')` is a supported statement whatever `t1` is. The engine checks each value against its column. It also gives SQLite's errors of the columns:

- `2 values for 1 columns`, when the list and the values differ in number.
- `table t1 has 3 columns but 2 values were supplied`, when a statement without a list has another number of values than the table has columns.
- `table t1 has no column named v9`, for a column of the list that the table does not have.
- `NOT NULL constraint failed: t1.v2`, with the extended result code 1299, for a column that says NOT NULL, or the primary key, when the list leaves it out or its value is NULL. The message names the first such column in the order of the table. A column that the list leaves out and that has a DEFAULT other than NULL gets its default and no error.

A column that takes NULL and has no DEFAULT gets NULL when the list leaves it out. A column with a DEFAULT gets its default, which is slice 11; see [CREATE TABLE](#create-table).

A value can also be a parameter, a plain `?`, which is slice 2. SQLite gives a `?` the number after the largest one before it, and every parameter of a supported statement is a plain `?`. So the k-th `?` of the statement has the number k. For a parameter, `Term.Parameter` is that number, `Term.Value` is nil, and `Term.Span` is the span of the `?`. `Parameters()` of every statement gives the number of its parameters, as `sqlite3_bind_parameter_count` gives it: the number of `?` in an insert, in the conditions and the LIMIT of a select, in the conditions of a delete, or in the assignments and the conditions of an update, and 0 for every other statement. A `?` in a subquery, which is slice 15, counts with those of the conditions. Which argument binds to a parameter, and what an argument may be, is for the engine and the driver to check; the pass sees only the text.

A column that the list names twice, as `Name.Fold` compares names, is not supported, as `column named twice`. SQLite accepts such a statement and uses the first value for the column: `insert into t (a, b, a) values (3, 'z', 4)` stores 3 in `a`.

Not supported: REPLACE, INSERT OR, WITH, an alias, DEFAULT VALUES, a second row, SELECT, a compound, and an upsert. Each stays not supported before a RETURNING, by the same name and at the same token; see [RETURNING](#returning).

Not supported as the value:

- A `+` sign, a second sign, `~`, and NOT.
- NULL after a sign, `-null` or `+null`, as `NULL after a sign`. SQLite reads both as NULL, and the roadmap lists only the plain word. The offset for `-null` is that of the NULL, as for a string after a sign, and the offset for `+null` is that of the `+`. The other names stay: `- -null` is `second sign`, `+-null` is `+ sign`, `~null` is `~ operator`, and `not null` as a value is `NOT operator`.
- A hex literal, and a digit separator, also in a real, as in `1_000.5`.
- An integer outside 64 bits. SQLite reads it as a real, and the pass rejects it and never reinterprets it.
- A real that SQLite reads as an infinity.
- A string or a blob after a `-`, as `string after a sign` and `blob after a sign`. SQLite gives the integer 0 for `select -'a'` and for `select -x'01'`.
- The bare word TRUE or FALSE, as `TRUE or FALSE`. SQLite reads them as 1 and 0 where no column of the name is.
- A name. A double-quoted string, such as `"a"`, is a name too: to the grammar it is a column reference, and it is not supported as `column reference`. The pinned build of SQLite has `DQS=0`, so SQLite gives `no such column: "a" - should this be a string literal in single-quotes?` for it.
- CURRENT_TIME and its kind, and parentheses, also around NULL.
- A parameter with a number or a name, and a parameter after a sign.
- A test for NULL: `is null` and `is not null`, as `IS`, and `isnull`, `notnull`, and `not null` after a value, as `NULL test`.
- Any other expression, such as `'a' || 'b'`, `cast('1' as int)`, `cast(null as text)`, `null collate binary`, or `nullif(1, 1)`.

A parameter can also be the value of a condition of a WHERE, which is slice 7, the value of an assignment of an update, which is slice 8, and the count of the LIMIT of a select, which is slice 13; see [SELECT](#select) and [UPDATE](#update). A parameter anywhere else is not supported for what holds it: a result column, a term of ORDER BY, a DEFAULT in parentheses, and so on. A `?` right after DEFAULT is a syntax error in SQLite, which the pass gives.

SQLite's own errors of a parameter are not reproduced, because every such parameter is not supported: `?0` and `?32767` give `variable number must be between ?1 and ?32766` in SQLite, and the pass gives `numbered parameter`. A `#` and a digit, as in `#1`, and two parameters without a comma between them, as in `(? ?)`, are syntax errors, which the pass gives as SQLite gives them.

### SELECT

```
SELECT * FROM table [WHERE condition [AND condition]...] [ORDER BY order-column [, order-column]...] [LIMIT count]
SELECT column [, column]... FROM table [WHERE condition [AND condition]...] [ORDER BY order-column [, order-column]...] [LIMIT count]
```

- `*` is every column, in the order of the table.
- A `column` is the name of a column alone, without a table before it, as SQLite reads it in the select list. The list can name the columns in any order, and it can name a column more than once.
- A keyword that SQLite reads as the name of a column in the select list is a name there, such as `abort`.
- WHERE, ORDER BY, and LIMIT come in that order, each at most once, and a select can have any of the three without the others. ORDER BY and LIMIT are slice 13, and this section defines them after the WHERE.

A `condition`, which is slice 7, is one of these:

```
column op value
value op column
column IS NULL
column IS NOT NULL
```

- `op` is one of `=`, `==`, `!=`, `<>`, `<`, `<=`, `>`, and `>=`. `=` and `==` are one operator, and `!=` and `<>` are one.
- `value` is a literal as [INSERT](#insert) defines it, of each of the five kinds, NULL too, with the same signs, or a plain `?`, or a scalar subquery, a select in parentheses, which is slice 15. [Subqueries](#subqueries) defines the subquery.
- The `column` is a bare name or a name in double quotes, without a table before it. A keyword that SQLite reads as the name of a column there is a name, such as `abort`. These are the same 84 keywords as in the select list, on either side of a comparison: `TestCheck` checks each against SQLite, which gives `no such column: kw` for a keyword that it reads as a column.
- `value op column`, which is slice 12, is a comparison with its value on the left, as in `5 < a` and `? >= timeout`. The pass reads the value with the code that reads a value on the right, and the column as it reads a column on the left. A test for NULL has its column on the left only.
- The pass gives a comparison with its value on the left as the condition with its column first and the operator swapped, and `Condition.ValueFirst` is true for it: `5 < a` is the condition `a > 5`, and `? >= timeout` is `timeout <= ?`. `<` and `>` swap, and so do `<=` and `>=`. `=` and `!=` stay. SQLite gives the two orders the same rows.
- A literal NULL on the left is supported: `null = a` gives no rows, as `a = null` does.
- Several conditions can test one column, in either order, as in `v > 1 and v < 5` and `1 < v and v < 5`.
- A `?` of a condition is a parameter, as in an insert: the k-th `?` of the statement has the number k, whichever side of its column it is on, and `Parameters()` counts them. `a = ? and ? < b and c > ?` numbers them 1, 2, and 3, as SQLite does. A `?` in a subquery has its number in its place in the text too.

The pass does not know the table, so these facts of SQLite are for the engine. `checkWhere` in the tests checks those of WHERE against SQLite; see [How it is tested](#how-it-is-tested).

- SQLite names a result column by the name as the table stores it, not as the statement writes it. With a column `b`, `select B from t` gives a column named `b`.
- For a name that the table does not have, SQLite gives `no such column: v`, with the value of the name. For such a name in double quotes, it gives `no such column: "v" - should this be a string literal in single-quotes?`, because the pinned build of SQLite has `DQS=0`.
- A comparison with NULL is unknown, and gives no rows: `v = null`, and `v = ?` with NULL as its argument.
- `value op column` gives the rows of `column op' value`, with `op'` the operator that `Operator.Swapped` gives, for every operator and for a value of every type, NULL too, as a literal and as a `?`. SQLite applies the affinity of the column to the value on either side. So the two orders agree also where SQLite converts the value, such as `'5' = a` on an INT column of a table that is not STRICT. A subtest of `TestCheck` checks this.
- SQLite looks for the names of the select list first, and then for those of WHERE, in the order of the text. The first comparison whose column the table does not have gives `no such column: v`, or the message for a name in double quotes.
- A comparison with its value on the left follows the same order: SQLite looks for its column in its place in the text, and gives the message for a name in double quotes for `5 < "nope"`. A value that is longer than the length limit gives `string or blob too big` after the errors of the names, on either side of its column. A subtest of `TestCheck` checks this, with the length limit at 1,000 bytes.
- A test for NULL of a column that the table does not have does not stop SQLite. `resolveExprStep` walks the operand of `TK_ISNULL` itself and ignores its abort. So a later test for NULL of a missing column replaces the message, and the next comparison stops the walk before SQLite looks for its own column. `a is null and b is null` gives `no such column: b`, and `a is null and b = 1` gives `no such column: a`.
- `rowid`, `oid`, and `_rowid_` give `no such column` in a WITHOUT ROWID table, unless the table has a column of that name.
- Without ORDER BY, the rows come in key order. SQLite searches the primary key for a condition on it, and scans the table in key order otherwise. It searches the key and an index for a comparison with its value on the left as for the other order: `EXPLAIN QUERY PLAN` gives `SEARCH t USING PRIMARY KEY (k>?)` for `5 < k`. This was measured with SQLite's shell only.

Not supported as a result column:

- `*` with another result column, as `* and another result column`.
- Any other result column, such as an expression, a literal, also NULL, `t.v`, or `t.*`, as `result column other than a column or *`.
- An alias, as `alias`.
- The bare word TRUE or FALSE, as `TRUE or FALSE`. SQLite reads it as 1 or 0 where the table has no column of that name. `"true"` in double quotes is a name.

Not supported: DISTINCT or ALL, no FROM, a second table or a join, an alias of the table, INDEXED BY or NOT INDEXED, a table in parentheses, a subquery in FROM, a table-valued function, ON or USING, GROUP BY, HAVING, WINDOW, a compound, WITH, VALUES. They stay not supported after a WHERE too, and before an ORDER BY or a LIMIT: `select * from t group by v order by v` is `GROUP BY clause`, and `select * from t union select * from t order by v` is `compound SELECT`.

Not supported in WHERE:

- A literal, a `?`, or a subquery on both sides of a comparison, also after a sign, as `value compared with a value`, at the value on the right: `1 = 1`, `? = ?`, `1 = ?`, `'v' = 1`, and `? = (select k from u)`.
- A literal, a `?`, or a subquery before IS, also after a sign, as `value before IS`, at the value: `5 is null`, `null is null`, `? is null`, `5 is not null`, `5 is v`, `5 is distinct from v`, and `(select k from u) is null`. `5 isnull` is `NULL test`.
- A column on both sides of a comparison, the one on the right also in double quotes or with its table, as `column compared with a column`, at the column on the right: `v = w`, `v = "a"`, and `v = t.w`.
- A column with its table before it, on either side of a comparison, as `column with the name of a table`, at the column: `t.v = 1` and `5 < t.v`.
- OR, as `OR`. The offset is that of the first OR, after the conditions before it.
- NOT before a condition, as `NOT operator`.
- Parentheses, around a condition, around a value, or around a column, as `parentheses`: `(5) < v` and `5 < (v)`.
- An expression or a function on either side, by the name of its node: `binary operator`, `unary operator`, `function call`, `CAST`, `CASE`, and `EXISTS`. A subquery was `subquery` there before slice 15. It is the value of a comparison now, and [Subqueries](#subqueries) lists the forms of one that are not supported.
- `IN`, `BETWEEN`, LIKE, GLOB, REGEXP, and MATCH, and COLLATE on either side.
- IS or IS NOT with anything but NULL. With a literal other than NULL, a parameter, a name, a value after a sign, or a subquery, it is `IS with a value other than NULL`, at the value: `v is 1`, `v is true`, `v is ?`, `v is w`, `v is -1`, and `v is (select k from u)`. Any other expression after IS has the name of its node: `v is (null)` is `parentheses`. IS DISTINCT FROM is `IS DISTINCT FROM`, at the DISTINCT, also after NOT.
- `isnull`, `notnull`, and `not null` after a column, as `NULL test`. SQLite reads them as IS NULL and IS NOT NULL, and the roadmap lists only `is null` and `is not null`.
- A column, a literal, a parameter, or a subquery alone as a condition, as `column reference`, `literal`, `parameter`, and `subquery`.
- The bare word TRUE or FALSE as a condition, as its column, or as its value, as `TRUE or FALSE`. `"true"` in double quotes is a name.
- A comparison of a comparison: `v = 1 = 1` and `5 < v < 10` are `binary operator`, and `v is null = 1` is `IS`.
- Every form of a value that INSERT does not support, by the name that it has there, such as `+ sign`, `second sign`, `numbered parameter`, `parameter after a sign`, and `NULL after a sign`.
- On the left of its column, every form of a value that the right side refuses, by the same name and at the same token: `+ sign`, `hex literal`, `TRUE or FALSE`, `parameter after a sign`, at the `?`, `NULL after a sign`, `string after a sign`, `blob after a sign`, `number with a digit separator`, `current date or time`, `infinite real number`, `integer outside 64 bits`, `numbered parameter`, and `named parameter`. Only a literal or a `?` with at most one sign is a value on the left: `- -5 = v`, `~1 < v`, and `-v = 1` are `unary operator`, at their first token, as before slice 12.

An update and a delete take the same WHERE, which is slice 8; see [UPDATE](#update) and [DELETE](#delete). A subquery takes a WHERE of the same conditions, without a subquery as a value; see [Subqueries](#subqueries). A WHERE anywhere else is not supported for what holds it: an INSERT with SELECT, CREATE TABLE AS, and a subquery in FROM.

ORDER BY, which is slice 13, comes after the WHERE, or after the table of a select without WHERE, and `Select.OrderBy` holds its columns. An `order-column` is one of these:

```
column
column ASC
column DESC
```

- The list has one or more columns, in any order.
- The `column` is a bare name or a name in double quotes, without a table before it. A keyword that SQLite reads as the name of a column there is a name, such as `abort`. These are the same 84 keywords as in the select list, first in the list or after another column, with ASC, DESC, or neither: `TestCheck` checks each against SQLite.
- DESC makes the column descending, and `OrderColumn.Descending` is true for it. ASC and neither are the same ascending order, and `Descending` is false for both.
- A column of ORDER BY need not be in the select list. SQLite accepts such a select, and the receive of goqite's queue is one: `select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1`. With `*`, the columns are those of the table.
- Each column is named once.

LIMIT, which is slice 13 too, comes last, after the ORDER BY, the WHERE, or the table, whichever the select has last, and `Select.Limit` holds its `count`:

- The `count` is a decimal integer literal without a sign that fits a signed 64-bit integer, or a plain `?`.
- Leading zeros are allowed, as in a value of an insert: `limit 007` is 7.
- `limit 0` is supported. It gives no row, in SQLite and in s4ql. The largest count is `9223372036854775807`.
- A `?` of LIMIT is a parameter, as in an insert. The k-th `?` of the statement has the number k, so the `?` of LIMIT comes after those of WHERE, as SQLite numbers them, and `Parameters()` counts it. `select v1 from t1 where v2 > ? order by v1 limit ?` numbers them 1 and 2.
- `Limit.Span` is the bytes of the literal, with its leading zeros, or of the `?`.

The pass does not know the table, so these facts of SQLite are for the engine. `checkWhere` and `checkOrder` in the tests check the first two on every supported text, and three subtests of `TestCheck` pin most of the others; see [How it is tested](#how-it-is-tested). [docs/todo/slice-13.md](../../../docs/todo/slice-13.md) has them all, with the arguments of `limit ?` and the plans that SQLite chooses.

- For a column of ORDER BY that the table does not have, SQLite gives `no such column: x`, or the message with the question about a string literal for a name in double quotes. It looks for the names of the select list first, then for those of WHERE, a missing column of a test for NULL among them, and then for those of ORDER BY, in the order of the text. `no such table` comes before all of them. s4ql has no alias, so `order by x` on a table without a column `x` is that error.
- The rows come in the order of the first column, each in its direction, and rows that tie there in the order of the next. With LIMIT, the rows are the first ones of that order, as many as the count says, or every row if the count is larger.
- NULL sorts first in ascending order and last in descending order, in a column of every type of a STRICT table.
- Integers sort by value, and so do reals. `-0.0` and `0.0` compare equal, so their rows tie.
- Texts sort by their bytes, which is the BINARY collation: `'B'` comes before `'a'`, and `'É'` before `'é'`. Blobs sort by their bytes, and a blob comes before a longer one that starts with it: `x'01'` comes before `x'0100'`.
- A column of a STRICT table holds one type and NULL, so values of two types never meet in ORDER BY. SQLite refuses a text in an INT or a REAL column, a blob in a TEXT column, and an integer in a BLOB column, with a message such as `cannot store TEXT value in INT column t.v` and the extended result code 3091. It converts an integer to the text or the real it stands for in a TEXT or a REAL column, which the exact types of slice 4 refuse before.
- SQLite gives rows that tie in the order in which it reads them, which is key order on a plain scan, in ascending and in descending order alike. s4ql varies their order, by the spec. The tests of the pass do not check the order of such rows.
- SQLite checks the count when the statement runs, at its first step, after every error of the names: `select k from t order by nope limit ?` with the argument `'x'` gives `no such column: nope`.

A column that ORDER BY names twice, as `Name.Fold` compares names, is not supported, as `column named twice`, at the later name: `order by v, v`, `order by v asc, w, V desc`, and `order by "v", v`. SQLite accepts such a statement and orders by the first, so the later one orders nothing: `order by a, a desc` gives the rows of `order by a`. The list of an INSERT, the SET of an UPDATE, and the columns of a CREATE INDEX refuse it too.

Not supported as a column of ORDER BY, each at the first token of its term but the last, with what SQLite does where it bears on the decision:

- A literal, a parameter, or an expression, as `ordering term other than a column`: `1`, `0`, `99`, `-1`, `0x1`, `1.5`, `'v'`, `x'00'`, `null`, `?`, `current_time`, `v + 1`, `-v`, `abs(v)`, `cast(v as int)`, and `(select 1)`. The roadmap rejects ordering by position and by an expression.
  - SQLite reads an integer literal that fits 32 bits as the position of a result column, also in hex, after a sign, in parentheses, or with COLLATE. `order by 2` orders by the second result column. `0`, `99`, `-1`, and `2147483647` give `1st ORDER BY term out of range - should be between 1 and N` when SQLite prepares the statement, on a database with the table.
  - Every other constant orders nothing: `2147483648`, `-2147483648`, `1.0`, `'a'`, `?`, `null`, and `true`.
  - A string in single quotes is a constant too, and never a name, as it is in CREATE INDEX. So `order by 'v'` is `ordering term other than a column`, and not `single-quoted name`.
- Parentheses around a column, `(v)`, as `parentheses`.
- A column with its table before it, `t.v` or `main.t.v`, as `column with the name of a table`.
- COLLATE after a column, `v collate nocase`, as `COLLATE`, at the column.
- The bare word TRUE or FALSE, as `TRUE or FALSE`. SQLite reads it as 1 or 0 where the table has no column of that name. `"true"` in double quotes is a name.
- A column in brackets or in backticks, and the empty name, as [Names](#names) says for every name.
- NULLS FIRST or NULLS LAST after a column, also after ASC or DESC, as `NULLS FIRST or NULLS LAST`, at the NULLS. SQLite takes both in ORDER BY, where it refuses them in CREATE INDEX.

The `count` of LIMIT follows the exact types of slice 4. SQLite takes any expression there, and converts its value to an integer when the statement runs:

- An integer in any form limits, and a real or a text that holds an integer is that integer. `1.0`, `1e2`, `'5'`, `'2.0'`, `0x5`, `1_0`, and `true` all limit.
- Every other value gives `datatype mismatch`, with the result code 20, at the first step, also on an empty table: `1.5`, `'x'`, `null`, `x'05'`, `9223372036854775808`, and `current_time`.
- A negative integer is no limit, and the select gives every row: `-1`, `0xffffffffffffffff`, and `-9223372036854775808`, and also `-1.0` and `'-1'`, which hold one.

The number of rows is an integer, so the pass refuses a literal of another type before the statement runs, also one that SQLite would convert. A negative number reads as "at most -1 rows" and means "every row", so the pass refuses it too. Only a literal that is an integer from 0, or a `?`, is a `count`. The pass sees no argument of a `?`. For a literal and an argument to agree, the engine should run an int64 from 0, and refuse a negative int64 and an argument of every other type. Which error it gives for each is decided when the engine is built.

Not supported as the `count` of LIMIT, each at its first token:

- A `-` before anything, as `negative LIMIT`: `-1`, `-0`, `- 5`, and `-?`.
- A `+` before anything, as `+ sign`: `+1`.
- A real, a string, a blob, or NULL, as `LIMIT other than an integer`: `1.5`, `1.0`, `1e2`, `'5'`, `x'05'`, and `null`.
- A form of an integer or of a parameter that a value of an INSERT refuses, by the name that it has there: `0x5` is `hex literal`, `9223372036854775808` is `integer outside 64 bits`, `1_0` is `number with a digit separator`, `?1` is `numbered parameter`, and `:n` is `named parameter`.
- `current_time` and its kind, as `current date or time`, the bare word TRUE or FALSE, as `TRUE or FALSE`, and parentheses, `(1)`, as `parentheses`.
- Anything else, as `LIMIT of an expression`: `1 + 1`, `~1`, `abs(1)`, and a name, bare or in double quotes. SQLite gives `no such column` for a name there, whatever the table holds.

So the names of a sign differ from those of a value of an INSERT. A `-` is `negative LIMIT` and a `+` is `+ sign`, whatever follows them. `second sign`, `parameter after a sign`, `NULL after a sign`, `string after a sign`, `blob after a sign`, `~ operator`, `NOT operator`, and `infinite real number` never name a LIMIT.

OFFSET is not supported, as `OFFSET`, in both forms that SQLite has. The roadmap rejects both.

- `limit 1 offset 2` is `OFFSET`, at the OFFSET, also with a `?` as either number.
- `limit 2, 1`, whose first number is the offset and whose second is the count, is `OFFSET`, at the comma.
- The pass reads the number before the OFFSET or the comma first, so `limit 'a', 1` is `LIMIT other than an integer`, at the string.

Syntax errors of ORDER BY and LIMIT, which the pass gives as SQLite gives them:

- ORDER BY without a column, `select * from t order by`, and a comma without a column after it, `order by v,`, which are `incomplete input`.
- ASC and DESC after one column, `order by v asc desc`, which is `near "desc": syntax error`.
- ORDER BY twice, which is `near "order": syntax error`.
- LIMIT without a count, `select * from t limit`, which is `incomplete input`.
- LIMIT before ORDER BY, `limit 1 order by v`, which is `near "order": syntax error`, and LIMIT twice, which is `near "limit": syntax error`.
- OFFSET without LIMIT, `select * from t offset 2`, which is `near "2": syntax error`. SQLite reads `offset` there as an alias of the table.

ORDER BY and LIMIT in an update and in a delete stay syntax errors; see [UPDATE](#update) and [DELETE](#delete).

### UPDATE

```
UPDATE table SET assignment [, assignment]... [WHERE condition [AND condition]...] [RETURNING returning-list]
```

An `assignment`, which is slice 8 as the statement is, is one of these:

```
column = value
column == value
```

- `=` and `==` are one token. SQLite's tokenizer gives both `TK_EQ`, the one token that the grammar takes there, so `set v == 1` means `set v = 1`.
- `value` is a literal as [INSERT](#insert) defines it, of each of the five kinds, NULL too, with the same signs, or a plain `?`.
- The `column` is a name as in the list of an INSERT: a bare name or a name in double quotes. A keyword that SQLite reads as a name there is a name, such as `abort`. These are the same keywords as in the list of an INSERT: `TestCheck` checks each against SQLite, which gives `no such column: kw` for a keyword that it reads as a name.
- The bare word TRUE or FALSE is a name there too: `update t set true = 1` assigns to the column `true`, and SQLite gives `no such column: true` for it on a table without that column. As the value of an assignment and in WHERE, the bare word stays `TRUE or FALSE`.
- `rowid`, `oid`, and `_rowid_` are names like any other.
- Each column is assigned once. A statement can assign to the column that its WHERE tests, as in `update t set v = 1 where v = 2`.
- The WHERE is that of [SELECT](#select), condition for condition, and the `value` of a comparison in it can be a scalar subquery, which is slice 15; see [Subqueries](#subqueries). A keyword is the name of the column of a condition if SQLite reads it as one in a WHERE, which holds for the same 84 keywords as in a select. So some keywords are names after SET and values in WHERE, such as `current_date`.
- RETURNING, which is slice 14, comes last, after the WHERE, or after SET in an update without WHERE. [RETURNING](#returning) defines it and its `returning-list`.
- A `?` of an assignment or of a condition is a parameter, as in an insert. The k-th `?` of the statement has the number k, so those of SET come before those of WHERE, as SQLite numbers them, and `Parameters()` counts both, those of the subqueries of the WHERE among them.
- SQLite's limits hold: SET has 2,000 assignments at most, and the WHERE is no deeper than that of a select; see [the errors that SQLite raises in grammar actions](#the-errors-that-sqlite-raises-in-grammar-actions).

The pass does not know the table, so these facts of SQLite are for the engine. `checkChange` in the tests checks them against SQLite; see [How it is tested](#how-it-is-tested).

- The errors of names come in this order. Without the table, SQLite gives `no such table: t`. Then it looks for the columns of SET, in the order of the text, and then for those of WHERE, by the rule of [SELECT](#select). `update t set z = 1 where y is null` gives `no such column: z` on a table that has neither.
- For the first column of SET that the table does not have, SQLite gives `no such column: v`, with the value of the name. It gives the same for a name in double quotes, without the question about a string literal in single quotes that it asks for a column of WHERE: `sqlite3Update` looks for the columns of SET itself, before the WHERE.
- A column named `rowid`, `oid`, or `_rowid_` in SET gives `no such column` with its name, such as `no such column: rowid`, in a WITHOUT ROWID table, unless the table has a column of that name. SQLite asks `sqlite3IsRowid` only of a table with a rowid.
- `changes()` counts every row that the WHERE matches, also a row whose values do not change. Without WHERE it counts every row of the table.

Three more facts are for the engine. They were measured with SQLite's shell only, and the tests of the pass do not check them:

- An update that gives a row the key of another row gives `UNIQUE constraint failed: t.a`, as an insert of the key does, and changes nothing: `update t set a = 2 where a = 1` when a row has the key 2, and `update t set a = 3` of two rows.
- `set c = null` of a column that says NOT NULL gives `NOT NULL constraint failed: t.c`.
- `set b = 1` of a TEXT column of a STRICT table stores the text `'1'`. So the exact types of slice 4 must refuse it in the engine.

A column that SET names twice, as `Name.Fold` compares names, is not supported, as `column named twice`, at the later name. SQLite accepts such a statement and uses the last value for the column, where it uses the first for an INSERT. The roadmap names neither.

Not supported:

- UPDATE OR with each of the five conflict clauses, such as `update or replace`, as `UPDATE OR`, at the OR.
- An assignment to a list of columns in parentheses, also a list of one, as `SET of a list of columns`, at the parenthesis: `set (v, w) = (1, 2)` and `set (v) = 1`.
- A 2,001st assignment, as `SET beyond SQLite's limit`.
- `update ... from`, as `FROM clause`, at the FROM.
- An alias of the table, as `alias`, INDEXED BY, as `INDEXED BY`, and NOT INDEXED, as `NOT INDEXED`.
- WITH before the statement, as `WITH clause`, EXPLAIN, as `EXPLAIN`, and EXPLAIN QUERY PLAN, as `EXPLAIN QUERY PLAN`.
- A table with a schema, `main.t` or `temp.t`, as `schema name`, as in INSERT and SELECT. SQLite takes `main.t`, and gives `no such table: temp.t` for `temp.t`.
- A column in brackets, in backticks, or in single quotes, and the empty name, as [Names](#names) says for every name. SQLite takes `update t set 'v' = 1`.

Not supported as the value of an assignment:

- Every form of a value that INSERT does not support, by the name that it has there, such as `+ sign`, `numbered parameter`, `named parameter`, `parameter after a sign`, `TRUE or FALSE`, `hex literal`, `infinite real number`, `current date or time`, and `NULL after a sign`.
- An expression, by the name of its node: `v = v + 1` is `binary operator`, `v = w` and `v = "a"` are `column reference`, `v = (1)` is `parentheses`, `v = abs(1)` is `function call`, and `v = (select 1)` and `v = (select k from u)` are `subquery`, at the parenthesis. A subquery is a value only in a comparison of a WHERE.
- The first token that is not supported decides the name: `update t set v = u.v from u` is `column reference`, at the value, before the FROM.

Not supported in the WHERE: everything that is not supported in the WHERE of a select, by the same names, with `WHERE deeper than SQLite's limit` and the names of [Subqueries](#subqueries) among them.

Syntax errors, which the pass gives as SQLite gives them:

- ORDER BY and LIMIT. The pinned build of SQLite lacks `SQLITE_ENABLE_UPDATE_DELETE_LIMIT`, so it gives `near "order": syntax error` and `near "limit": syntax error`. The roadmap lists them as not supported. In this build SQLite's syntax error comes first, as for every syntax error.
- `<>` in place of `=`, as in `set v <> 1`, DEFAULT as the value, and a table before the column, as in `set t.v = 1`.

### DELETE

```
DELETE FROM table [WHERE condition [AND condition]...] [RETURNING returning-list]
```

- Without a WHERE, the statement deletes every row of the table.
- The WHERE is that of [SELECT](#select), condition for condition, and the same 84 keywords are names of its columns. The `value` of a comparison in it can be a scalar subquery, which is slice 15; see [Subqueries](#subqueries).
- RETURNING, which is slice 14, comes last, after the WHERE, or after the table in a delete without WHERE. [RETURNING](#returning) defines it and its `returning-list`.
- A `?` of a condition is a parameter, as in a select: the k-th `?` of the statement has the number k, also in a subquery, and `Parameters()` counts them.

These facts of SQLite are for the engine, and `checkChange` in the tests checks them against SQLite:

- Without the table, SQLite gives `no such table: t`. Then it looks for the columns of WHERE, by the rule of [SELECT](#select), with the question about a string literal for a name in double quotes.
- `changes()` counts every row that the statement deletes: every row that the WHERE matches, or every row of the table without WHERE.

Not supported:

- An alias of the table, as `alias`, INDEXED BY, as `INDEXED BY`, and NOT INDEXED, as `NOT INDEXED`. `delete from t as u where u.v = 1` is `alias`, at the AS.
- WITH before the statement, as `WITH clause`, EXPLAIN, as `EXPLAIN`, and EXPLAIN QUERY PLAN, as `EXPLAIN QUERY PLAN`.
- A table with a schema, `main.t` or `temp.t`, as `schema name`.
- In the WHERE, everything that is not supported in the WHERE of a select, by the same names: `delete from t where 1 = 1` is `value compared with a value`, and `delete from t where t.v = 1` is `column with the name of a table`.

Syntax errors, which the pass gives as SQLite gives them: ORDER BY and LIMIT, as for an UPDATE; a statement without FROM, `delete t`; and a second table, `delete from t, u`.

### RETURNING

```
RETURNING *
RETURNING column [, column]...
```

RETURNING, which is slice 14, is the last clause of an insert, an update, or a delete, and the `Returning` of the statement holds it. What follows the keyword is the `returning-list` of [INSERT](#insert), [UPDATE](#update), and [DELETE](#delete). SQLite's grammar takes a select list there, and the pass reads each column with the code that reads a column of the select list of [SELECT](#select). So the two support the same forms, and refuse every other form under the same name.

- `*` alone is every column, in the order of the table.
- A `column` is the name of a column alone, without a table before it: a bare name or a name in double quotes. `"*"` in double quotes is a name.
- The list has one or more columns, in any order. A column need not be one that the statement names before: `insert into t (v) values (1) returning w` is supported.
- The list can name a column more than once, as a select list can, and the statement gives the column as often: `returning v, V, v` gives three columns. The list of an INSERT, the SET of an UPDATE, the columns of a CREATE INDEX, and an ORDER BY refuse a column named twice, because the later one does nothing there. A later column of RETURNING is a column of the result.
- A keyword that SQLite reads as the name of a column there is a name, such as `abort`. These are the same 84 keywords as in the select list, after an insert, an update, and a delete, first in the list and after another column: `TestCheck` checks each against SQLite.
- `rowid` is a name like any other.
- RETURNING holds no parameter. A `?` there is not supported, and `Parameters()` counts those of the rest of the statement.

The pass does not know the table, so these facts of SQLite are for the engine. `checkInsertReturning` and `checkChangeReturning` in the tests check the rows on every supported text, and `checkReturningMissing` and `checkChange` the first missing column. A subtest of `TestCheck` pins the order of the errors and the failures of constraints; see [How it is tested](#how-it-is-tested).

- SQLite names a result column of RETURNING by the name as the table stores it, as in a select. With a column `K`, `returning k` gives a column named `K`, and `returning k, K` gives two of that name.
- An insert gives one row, with the values that a select of the same columns reads after it: the DEFAULT of a column that the list leaves out, or NULL for a column without one, and `0.0` for a `-0.0` in a REAL column.
- An update gives one row for each row that its WHERE chooses, as many as `changes()` counts, also for a row whose values do not change. The row has the values after the update, also of the key: `update t set k = 100 where k = 1 returning k` gives 100.
- A delete gives one row for each row that it deletes, with the values that the row had.
- SQLite looks for the columns of RETURNING last, after every other error of the names. `no such table: t` comes first. For an insert, the errors of its list and its values come next: `table t has no column named x`, `1 values for 2 columns`, and `table t has 2 columns but 1 values were supplied`. For an update, the columns of SET come next. For an update and a delete, the columns of WHERE follow, by the rule of [SELECT](#select). A missing column of a test for NULL, which does not stop SQLite in the WHERE, gives its error before RETURNING all the same: `update t set a = 1 where nope is null returning nope2` gives `no such column: nope`.
- For the first column of RETURNING that the table does not have, in the order of the text, SQLite gives `no such column: x`, or `no such column: "x" - should this be a string literal in single-quotes?` for a name in double quotes. `returning rowid` gives `no such column: rowid` in a WITHOUT ROWID table.
- A failure of a constraint comes when the statement runs, after every error of the names. SQLite then gives no row and changes nothing. An insert of a key that exists, and an update that gives a row the key of another, give `UNIQUE constraint failed: t.k`, with the extended result code 1555. An insert of NULL as the key gives `NOT NULL constraint failed: t.k`, with the extended result code 1299. With a missing column of RETURNING, the error of the name comes: `insert into t (k) values (null) returning nope` gives `no such column: nope`.

Four more facts are for the engine. They were measured with SQLite's shell only, and the tests of the pass do not check them:

- SQLite gives the rows of an update and of a delete in the order in which it changes them, which is key order on a plain scan. s4ql promises no order, by the spec's rule for a select without ORDER BY.
- More than 2,000 columns in RETURNING give `too many columns in result set` when SQLite prepares the statement, as more than 2,000 columns in a select list do. 2,000 is `SQLITE_MAX_COLUMN`. The pass does not check it, as it does not for a select list, so the error is the engine's to give. [docs/todo/sql-front-end.md](../../../docs/todo/sql-front-end.md) has the open item.
- Under `pragma query_only`, as a read-only transaction of s4ql runs it, an insert, an update, and a delete with RETURNING give `attempt to write a readonly database`.
- `changes()` after an insert with RETURNING is 1, as without it.

Not supported in RETURNING, each at the first token of its column but the alias, with what SQLite does where it bears on the decision:

- `*` with another column, as `* and another result column`, at the first `*`: `returning *, v`, `returning v, *`, and `returning *, *`. SQLite takes all three, and gives the columns of `*` and the others in the order of the text. This is the rule of the select list, which slice 5 decided.
- An expression, a function, a literal, a parameter, parentheses around a column, and COLLATE after a column, as `result column other than a column or *`: `v + 1`, `abs(v)`, `1`, `null`, `'v'`, `?`, `(v)`, and `v collate nocase`. SQLite reads a string in single quotes as a string there, and never as a name, so `returning 'v'` is not `single-quoted name`.
- A column with the name of its table, and `*` with one, as `result column other than a column or *` too: `t.v`, `main.t.v`, `excluded.v`, and `t.*`. SQLite takes `t.v`. When it prepares the statement, it refuses `t.*` with `RETURNING may not use "TABLE.*" wildcards`, and `main.t.v` with `no such column: main.t.v`.
- An alias, with AS or without it, as `alias`, at the AS or at the alias: `returning v as x` and `returning v x`.
- The bare word TRUE or FALSE, as `TRUE or FALSE`. SQLite reads `true` as 1.
- A name in brackets or in backticks, and the empty name, as [Names](#names) says for every name.

What the pass refuses before a RETURNING stays as it was, by the same name and at the same token. `insert into t (v) values (1) on conflict do nothing returning *` is `ON CONFLICT clause`, at the ON. `insert into t default values returning *` is `DEFAULT VALUES`, a second row before RETURNING is `second row`, and an insert with SELECT is `INSERT with SELECT`. `update or replace t set v = 1 returning *` is `UPDATE OR`, and `update t set v = 1 from u returning *` is `FROM clause`. WITH and EXPLAIN before the statement are `WITH clause` and `EXPLAIN`. A statement after the RETURNING is `second statement`.

Syntax errors of RETURNING, which the pass gives as SQLite gives them:

- RETURNING without a column, which is `incomplete input` at the end of the text, and `near ";": syntax error` before a semicolon.
- A comma without a column after it, `returning v,`, which is `incomplete input`.
- RETURNING twice, `returning v returning w`, which is `near "returning": syntax error`.
- DISTINCT after RETURNING, `returning distinct v`, which is `near "distinct": syntax error`.
- RETURNING before the WHERE of an update or of a delete, `delete from t returning * where v = 1`, which is `near "where": syntax error`.
- ORDER BY after RETURNING, `update t set v = 1 returning v order by v`, which is `near "order": syntax error`.
- RETURNING after a select, a create table, a drop table, a create index, or a drop index, and in a statement of the body of a trigger, which are `near "returning": syntax error`.

### Subqueries

```
( SELECT * FROM table [WHERE condition [AND condition]...] [ORDER BY order-column [, order-column]...] [LIMIT count] )
( SELECT column FROM table [WHERE condition [AND condition]...] [ORDER BY order-column [, order-column]...] [LIMIT count] )
```

A scalar subquery, which is slice 15, is a select in parentheses that stands where one value stands. It is supported as the `value` of a comparison in the WHERE of a select, an update, or a delete, and the `Subqueries` of the statement hold its select. The slice was agreed for the receive of goqite's queue, without the arithmetic in its SET: `update goqite set timeout = ? where queue = ? and id = (select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1) returning id, body, received`.

The pass reads the value of every comparison with one function, `recognizer.conditionValue`, and the select in the parentheses with the code that reads a select statement, `recognizer.selectOf`. So a subquery is supported in every place where a `?` is supported as the value of a comparison of such a WHERE, and its select supports the forms of [SELECT](#select) and refuses every other form under the same name.

- The subquery can be on either side of its column, with each of the six operators in each of their eight spellings: `k = (select k from u)` and `(select k from u) < k`. For a subquery on the left, the pass gives the condition with its column first and the operator swapped, and `Condition.ValueFirst` is true, as for every value on the left.
- A WHERE can have several subqueries, each as the value of a comparison of its own, beside comparisons with a literal or a `?` and tests for NULL.
- The select has one result column: the name of a column alone, as in the select list of [SELECT](#select), or `*`. `*` is one column on a table of one column. The pass does not know the table, so it supports `*`, and the error for a table of more columns is the engine's to give.
- The select reads one table, which can be the table of the statement: `delete from t where k = (select k from t order by k desc limit 1)`.
- The select can have a WHERE, an ORDER BY, and a LIMIT, each as [SELECT](#select) defines it: conditions joined by AND, with a literal or a `?` on either side of a column and with tests for NULL, columns with ASC, DESC, or neither, and a count that is an integer from 0 or a `?`. `limit 0` is supported.
- The value of a condition of a subquery is a literal or a `?`, and never a subquery.
- A name of a subquery is a name as in a select: a bare name, a name in double quotes, or a keyword that SQLite reads as a name there, as in `abort = (select key from abort where x = -1)`.
- A `?` in a subquery is a parameter, with its number in the order of the text. `select * from t where a = ? and b = (select b from u where c = ? and ? < d order by e limit ?) and f = ? order by g limit ?` numbers the `?` of `a` 1, the three of the subquery 2, 3, and 4, that of `f` 5, and that of the last LIMIT 6, as SQLite does.
- Any spaces and comments may come in the gaps, also after the opening parenthesis and before the closing one. `Term.Span` is from the opening parenthesis to the end of the closing one, and the `Span` of the select from its SELECT to the end of its last token.
- SQLite's limit of the depth of an expression holds, and a subquery counts towards it twice; see [the errors that SQLite raises in grammar actions](#the-errors-that-sqlite-raises-in-grammar-actions).

The pass does not know the tables, so these facts of SQLite are for the engine. `checkSubqueries` in the tests checks the value, and that it comes from the rows as they are before the statement, on every supported text with a subquery. A subtest of `TestCheck` pins the others, but for what the list says was measured with SQLite's shell only; see [How it is tested](#how-it-is-tested). [docs/todo/slice-15.md](../../../docs/todo/slice-15.md) has them too, and [docs/todo/sql-front-end.md](../../../docs/todo/sql-front-end.md) has the items that are open for the engine.

- The value of a subquery is the value of its one column in the first row that the subquery gives, or NULL if it gives no row. A comparison with NULL chooses no row. With several rows and no ORDER BY, SQLite gives the first in its order of reading, which is key order on a plain scan. `limit 0` gives no row, and so NULL.
- SQLite computes the value once, before the statement changes any row, also when the subquery reads the table that an update or a delete changes. On a table `t` of three rows with `v = 0`, `update t set v = 9 where v = (select v from t order by k limit 1)` changes all three.
- SQLite computes the value only when it needs it, so whether an error of a subquery shows depends on SQLite's plan. With `limit ?` bound to NULL, which is `datatype mismatch`, `select * from t where v = (select k from u limit ?)` gives no error on an empty `t`. `select * from t where k = (select k from u limit ?)` gives the error also on an empty `t`, since SQLite needs the value to search the key. Measured with the shell only: `where k = 5 and v = (...)` gives no error when no row has the key 5.
- SQLite reads the LIMIT X of a scalar subquery as LIMIT (X <> 0). With `limit ?`, the arguments `'x'`, `-1`, and `1.5` give the first row, `0.0` gives no row, and NULL gives `datatype mismatch`. Measured with the shell only: `x'00'` gives the first row, and the integer 0 no row. A literal `count` of the pass is an integer from 0, for which the two readings agree. The rule of slice 13 for an argument of `limit ?`, an integer from 0 and nothing else, would be stricter than SQLite here. Which of the two the engine gives is decided when the engine is built.
- `no such table` comes first. In a select, SQLite finds every table of the statement before any column: that of the statement, and then those of the subqueries in the order of the text. `select * from t where nope = (select k from nope2)` and `select nope from t where k = (select k from nope2)` give `no such table: nope2`. In an update and a delete, SQLite finds the table of a subquery only when its walk over the WHERE reaches the subquery: `update t set v = 1 where nope = 1 and k = (select k from nope2)` gives `no such column: nope`, and `update t set v = 1 where k = (select k from nope2) returning nope` gives `no such table: nope2`.
- The errors of the columns come in the order of the text, and those of a subquery where the subquery is. `k = (select nope from u) and nope2 = 1` gives `no such column: nope`, and `nope2 = 1 and k = (select nope from u)` gives `no such column: nope2`. A missing column of the ORDER BY of a subquery gives `no such column` too. The columns of SET come before those of a subquery, and those of RETURNING after them.
- `*` on a table of more than one column is `row value misused` in a select, with the subquery on either side. In an update and a delete it is `sub-select returns 2 columns - expected 1`, with the number of the columns, but for `!=` and `<>` with the subquery on the left, where it is `row value misused` too. The error comes after those of the names of WHERE, and before those of the names of RETURNING: `delete from t where k = (select * from u) and nope = 1` gives `no such column: nope`.
- SQLite looks for a name of a subquery in the table of the subquery first, and then in the table of the statement, without an error. When `u` has no column `v` and `t` has one, the subquery of `select * from t where v = (select v from u)` gives the `v` of the row of `t` that the WHERE tests: on a `t` of three rows with `v = 0` and a `u` with rows, the select gives all three. `order by v` in such a subquery orders by the `v` of the statement. Whether a name is the subquery's own depends on the tables, so the pass does not detect such a correlated subquery, and the slice does not name one. A column with the name of its table, `t.v`, and a column compared with a column, `x = v`, are refused already. So the only names that can be correlated are the bare result column, the column of a condition, and a column of the ORDER BY of the subquery.
- Measured with the shell only: under `pragma query_only`, as a read-only transaction of s4ql runs it, a select with a subquery runs, and an update with one gives `attempt to write a readonly database`.

Not supported in the select of a subquery, each at its first token unless the list says otherwise, with what SQLite does where it bears on the decision:

- A second result column, as `subquery of more than one column`, at the second column: `(select k, x from u)` and `(select k, * from u)`. SQLite refuses such a statement on every database. It gives `row value misused`, in a select, an update, and a delete alike, also when the list names a column that the table lacks, and it gives `no such table` first in a select whose subquery reads a table that is missing. So the pass differs from SQLite only in the error, and not in what runs. It follows the rule of `index beyond SQLite's limit`: an error that SQLite gives once it has found the table, and that the pass gives as not supported.
- `*` with another column after it, as `* and another result column`, at the `*`: `(select *, k from u)`. This is the rule of the select list.
- A result column that is not the name of a column or `*`, as `result column other than a column or *`: `(select 1)`, `(select k + 1 from u)`, and `(select max(k) from u)`.
- A subquery as the value of a condition of the subquery, on either side of its column, as `subquery in a subquery`, at the parenthesis of the inner subquery: `(select k from u where x = (select x from w))` and `(select k from u where (select x from w) < x)`. After a `-` and after IS, the inner subquery has the names that a subquery has there in the WHERE of a statement, `subquery after a sign` and `IS with a value other than NULL`.
- A compound select, with UNION, UNION ALL, EXCEPT, or INTERSECT, as `compound SELECT`, at its operator.
- A subquery in the FROM of the subquery, as `subquery in FROM`, in its ORDER BY, as `ordering term other than a column`, and in its LIMIT, as `LIMIT of an expression`.
- Everything else that a select refuses, by the name that it has there: WITH is `WITH clause`, DISTINCT is `DISTINCT or ALL`, GROUP BY is `GROUP BY clause`, an alias is `alias`, a table with a schema is `schema name`, a second table is `join`, at the comma, OFFSET is `OFFSET`, OR is `OR`, a column with the name of its table is `column with the name of a table`, a numbered parameter is `numbered parameter`, and VALUES in place of the select is `VALUES`. A select without FROM, `(select k)`, is `SELECT without FROM`, at the closing parenthesis.

Not supported around a subquery in a WHERE, each at its first token unless the list says otherwise:

- A subquery after a `-`, as `subquery after a sign`, at the parenthesis of the subquery: `k = -(select k from u)`. After a `+` it is `+ sign`, at the `+`, as for a parameter.
- A subquery in parentheses, `((select k from u))`, as `parentheses`, and a subquery with COLLATE after it, as `COLLATE`.
- A subquery compared with a literal, a parameter, or a subquery, as `value compared with a value`, at the value on the right: `? = (select k from u)`, `(select k from u) = 1`, and `(select k from u) = (select k from u)`.
- A subquery before IS, as `value before IS`, and a subquery after IS, as `IS with a value other than NULL`, at the subquery. `v is (select k from u)` was `subquery` before slice 15.
- A subquery alone as a condition, `where (select k from u)`, as `subquery`.
- A comparison of a comparison with a subquery, `k = (select k from u) = 1`, as `binary operator`, at the first token of the condition.
- `in (select ...)` and `not in (select ...)`, as `IN`, at the first token of the condition. `exists (select ...)` is `EXISTS`, and `not exists (select ...)` is `NOT operator`.
- A subquery that makes the WHERE deeper than SQLite's limit, as `WHERE deeper than SQLite's limit`; see [the errors that SQLite raises in grammar actions](#the-errors-that-sqlite-raises-in-grammar-actions).

A subquery anywhere but in a comparison of a WHERE is not supported, by the name that its place gives it, at its parenthesis unless the list says otherwise:

- In FROM, as `subquery in FROM`.
- In the select list and in RETURNING, as `result column other than a column or *`.
- In SET and in VALUES, as `subquery`.
- In ORDER BY, as `ordering term other than a column`.
- In LIMIT, as `LIMIT of an expression`.
- In a DEFAULT, which only `default ((select 1))` can write, as `DEFAULT of an expression`, at the first parenthesis. SQLite gives `default value of column [v] is not constant` for it while it parses.

Every form that SQLite takes and the slice does not name stays not supported: IN, EXISTS, a subquery outside a comparison of a WHERE, a subquery in a subquery, a compound select, and a subquery after a sign.

The first token that is not supported decides the name, so a text whose first such token is inside a subquery has the name for that token. `select * from t where v = (select 1)` is `result column other than a column or *`, at the `1`. Before slice 15 it was `subquery`, at the parenthesis.

Syntax errors of a subquery, which the pass gives as SQLite gives them:

- A subquery without its closing parenthesis, `where k = (select k from u`, which is `incomplete input`.
- A closing parenthesis too many, `where k = (select k from u))`, which is `near ")": syntax error`.
- Empty parentheses as a value, `where k = ()`, and SELECT alone in them, `where k = (select)`, which are `near ")": syntax error`.
- A select in the parentheses of a DEFAULT, `default (select 1)`, which is `near "select": syntax error`.

### DROP TABLE

```
DROP TABLE [IF EXISTS] table
```

IF EXISTS, which is slice 10, comes between TABLE and the name of the table, and `DropTable.IfExists` is true for it:

- The clause is the two keywords IF and EXISTS, each in any case of its letters, with any spaces and comments between them.
- After the clause, 89 keywords are names of the table: the 88 of the plain statement, and `if`, which starts the clause in the plain statement. So `drop table if exists if` is supported, and SQLite drops the table `if` for it. `TestCheck` checks each keyword against SQLite.

The pass does not know the database, so these facts of SQLite are for the engine. The DROP TABLE case of `checkMeaning` and a subtest of `TestCheck` check them against SQLite; see [How it is tested](#how-it-is-tested).

- Without a table of the name, the statement does nothing and gives no error, also when an index has the name. With the table, it drops the table as the plain statement does.
- A statement that does nothing writes nothing. It leaves SQLite's count of changes as it was, as a statement that drops the table does.
- In a read-only transaction, a statement that does nothing succeeds, and a statement that would drop the table gives `attempt to write a readonly database`.

Not supported: DROP VIEW, DROP TRIGGER, and a schema name, each with IF EXISTS as without it. `drop view if exists v` is `DROP VIEW`, and `drop table if exists main.t` is `schema name` at the `main`. A name that the pass refuses stays refused after the clause, also where SQLite does nothing: SQLite gives no error for `drop table if exists nosuch.t`, where no database `nosuch` exists, and none for `drop table if exists sqlite_x`, which are `schema name` and `reserved table name`. For `drop table if exists sqlite_schema` SQLite gives `table sqlite_master may not be dropped`.

Syntax errors of the clause, which the pass gives as SQLite gives them: the clause after the name, `drop table t if exists`, which is `near "if": syntax error`; IF NOT EXISTS in place of IF EXISTS, which is `near "not": syntax error`; IF alone before the name, `drop table if t`, which is `near "t": syntax error`; the clause twice, and a table named `exists` after the clause, which are `near "exists": syntax error`; and a second table, `drop table if exists t, u`, which is `near ",": syntax error`.

### CREATE INDEX

```
CREATE INDEX [IF NOT EXISTS] index ON table ( column [ASC|DESC] [, column [ASC|DESC]]... )
```

- The index is not unique.
- IF NOT EXISTS, which is slice 10, comes between INDEX and the name of the index, as the three keywords that it is in [CREATE TABLE](#create-table), and `CreateIndex.IfNotExists` is true for it. `CreateIndex.SQL` is the same with the clause and without it: SQLite keeps `CREATE INDEX IF/**/NOT EXISTS/* x */I ON T (A DESC)` as `CREATE INDEX I ON T (A DESC)`.
- `index`, `table`, and `column` are names: each is a bare name or a name in double quotes. A `column` is the name of a column of the table alone, without a table before it.
- After a column comes ASC, DESC, or neither. DESC makes the column descending in the index, and `pragma_index_xinfo` gives `desc` 1 for it. ASC and neither make the same index, and `IndexColumn.Descending` is false for both.
- The list has from 1 to 2,000 columns, in any order. 2,000 is SQLite's limit, `SQLITE_MAX_COLUMN`; see [the errors that SQLite raises in grammar actions](#the-errors-that-sqlite-raises-in-grammar-actions).
- Each column is named once.
- A keyword that SQLite reads as a name in a place is a name there, such as `abort`. That holds for 88 keywords as the name of the index, with IF NOT EXISTS before it or without, for 89 as the table after ON, where `if` is a name too, and for 84 as a column, the same 84 as in the select list. `TestCheck` checks each against SQLite.
- The bare word IF as the name of the index after IF NOT EXISTS, in any case of its letters, is not supported, as `bare name IF after IF NOT EXISTS`, at the name. SQLite's parser accepts `create index if not exists if on t (v)`. SQLite then keeps `CREATE INDEX if on t (v)`, which it cannot parse again: it gives `malformed database schema (if) - near "on": syntax error` and creates nothing. `"IF"` in double quotes is a name like any other, and so is the bare `if` as the table after ON and as a column: `create index if not exists "IF" on if (if)` is supported.
- `rowid`, `oid`, and `_rowid_` are names of columns like any other.
- The index can have the name of a table, also that of its own table, and the name of an eponymous virtual table of SQLite. The table after ON follows the rules of a table name. [Names](#names) has both.

The pass does not know the database, so these facts of SQLite are for the engine. `checkCreateIndex` in the tests checks them against SQLite; see [How it is tested](#how-it-is-tested).

- The errors come in this order, each with the result code 1. Without the table, SQLite gives `no such table: main.t`, with `main.` before the value of the name of the table, also for a name in double quotes.
- Then SQLite looks for a table of the name of the index, in any case of the ASCII letters, and gives `there is already a table named i`, with the value of the name of the index as the statement writes it. The table of the statement is such a table: `create index t on t (a)` gives this error on every database that has `t`. So SQLite never creates an index of the name of its table, and the pass supports the statement, as it supports a CREATE TABLE of a table that exists.
- Then it looks for an index of the name, in any case of the ASCII letters, on any table, and gives `index i already exists`, with that value too.
- Then it looks for the columns, in the order of the text. The first that the table does not have gives `no such column: z` for a bare name, and `no such column: "z" - should this be a string literal in single-quotes?` for a name in double quotes, as in a WHERE.
- SQLite records the columns of the index in the order of the text, each under the name as the table stores it and with the collation `BINARY`. After them it records the primary key of the table, which `pragma_index_xinfo` gives with `key` 0.
- With IF NOT EXISTS, SQLite does nothing and gives no error where it gives `index i already exists` without the clause: with an index of the name, in any case of the ASCII letters, on any table. It does not look for the columns then, so a statement whose table lacks a column of the list does nothing too. The two errors that come before stay, `no such table: main.t` and `there is already a table named i`. Without an index of the name, every error comes as for the plain statement, `no such column` among them.
- A statement that does nothing writes nothing, and it leaves SQLite's count of changes as it was, as a statement that creates the index does. In a read-only transaction it succeeds, and a statement that would create the index gives `attempt to write a readonly database`, after the errors of the names and of the columns. A subtest of `TestCheck` checks this and the bullet before it on a database with the table and the index.

Five more facts are for the engine. They were measured by hand, the last four with SQLite's shell only, and the tests of the pass do not check them:

- `no such column` has the offset of the name, and the other errors have the offset -1.
- `drop table` drops the indexes of the table with it. A table of the same name and an index of the same name can be created after it.
- A CREATE INDEX or a DROP INDEX in a transaction that rolls back is undone with it.
- The index of a WITHOUT ROWID table has the columns of the index and then those of the primary key that the index does not have already (`isDupColumn`). So an index `(b desc, a)` on a table with the primary key `a` has the key `b desc, a` and nothing after it.
- An index on a table with rows is built from them. It is not unique, so no row can make it fail.

A column that the list names twice, as `Name.Fold` compares names, is not supported, as `column named twice`, at the later name. SQLite accepts such a statement and makes an index of the column twice: `(a, a)` has two key columns `a`, and `(a, a desc)` has both orders. The roadmap leaves this to the slice.

Not supported, with what SQLite does where it bears on the decision:

- `create unique index`, as `UNIQUE INDEX`, at the UNIQUE.
- A schema on the name of the index, `main.i` or `temp.i`, as `schema name`. SQLite creates `main.i`, and gives `cannot create a TEMP index on non-TEMP table "t"` for `temp.i`.
- A WHERE after the columns, as `partial index`, at the WHERE.
- A column that is an expression, as `index on an expression`, at its first token: `a + 1`, `-a`, `lower(a)`, NULL, CURRENT_TIME, a number, and a parameter.
- COLLATE after a column, as `COLLATE`, at the column.
- Parentheses around a column, `((a))`, as `parentheses`. SQLite reads it as the column `a`.
- A column with its table before it, `t.a` or `main.t.a`, as `column with the name of a table`. SQLite gives `the "." operator prohibited in index expressions` once it has found the table.
- The bare word TRUE or FALSE as a column, as `TRUE or FALSE`. SQLite reads it as 1 or 0 where the table has no column of that name, which is an index on an expression. `"true"` in double quotes is a name.
- A column in single quotes, `('a')`, as `single-quoted name`, as [Names](#names) says for every name. SQLite reads it as the name of the column and never as an expression (`sqlite3StringToId`): `('zz')` gives `no such column: zz`.
- NULLS FIRST or NULLS LAST after a column, also after DESC, as `NULLS FIRST or NULLS LAST`, at the NULLS. SQLite refuses both on any database.
- A 2,001st column, as `index beyond SQLite's limit`.
- An index name that is reserved, as `reserved index name`; see [Names](#names).
- REINDEX and ANALYZE, with a name or without one, as `REINDEX` and `ANALYZE`, at their first token.
- EXPLAIN before the statement, as `EXPLAIN`, and EXPLAIN QUERY PLAN, as `EXPLAIN QUERY PLAN`.

A form that is not supported without IF NOT EXISTS is not supported with it, by the same name and at the same token: `create unique index if not exists i on t (v)` is `UNIQUE INDEX` at the UNIQUE, and `create index if not exists i on t (v, v)` is `column named twice` at the second `v`. This holds also where SQLite does nothing because the index exists: the pass sees only the text.

Syntax errors, which the pass gives as SQLite gives them:

- An empty list, `create index i on t ()`, which is `near ")": syntax error`.
- `create temp index`, which is `near "index": syntax error`.
- A schema on the table after ON, as in `on main.t`, which is `near ".": syntax error`. The grammar takes one name there.
- WITH before the statement, which is `near "create": syntax error`.
- IF NOT EXISTS before INDEX, `create if not exists index i on t (v)`, which is `near "if": syntax error`.
- IF EXISTS in place of IF NOT EXISTS, which is `near "exists": syntax error`.
- IF NOT or IF alone before the name, `create index if not i on t (v)` and `create index if i on t (v)`, which are `near "i": syntax error`.
- IF NOT EXISTS twice, and IF NOT EXISTS after ON, `create index i on if not exists t (v)`, which are `near "not": syntax error`. After ON, `if` is the name of the table.
- An index named `if` without the clause, `create index if on t (v)`, which is `near "on": syntax error`: IF starts the clause there.

### DROP INDEX

```
DROP INDEX [IF EXISTS] index
```

- The `index` is a bare name or a name in double quotes. A keyword that SQLite reads as a name there is a name, such as `abort`, which holds for 88 keywords. `TestCheck` checks each against SQLite, which gives `no such index: kw` for a keyword that it reads as a name.
- The name of an eponymous virtual table of SQLite, such as `pragma_table_list`, is a name like any other; see [Names](#names).
- IF EXISTS, which is slice 10, comes between INDEX and the name of the index, as the two keywords that it is in [DROP TABLE](#drop-table), and `DropIndex.IfExists` is true for it. After the clause, 89 keywords are names of the index: the 88 of the plain statement, and `if`, which starts the clause in the plain statement. So `drop index if exists IF` is supported.

These facts of SQLite are for the engine, and `checkDropIndex` in the tests checks them against SQLite:

- Without the index, SQLite gives `no such index: i`, with the value of the name and the result code 1, also when a table has the name. Its offset is -1, which was measured by hand and which the tests of the pass do not check.
- With IF EXISTS, SQLite does nothing and gives no error where it gives `no such index` without the clause, also when a table has the name.
- A statement that does nothing writes nothing, and it leaves SQLite's count of changes as it was, as a statement that drops the index does. In a read-only transaction it succeeds, and a statement that would drop the index gives `attempt to write a readonly database`. A subtest of `TestCheck` checks this on a database with a table and an index.

Not supported, with IF EXISTS as without it:

- A schema on the name, `main.i` or `temp.i`, as `schema name`. SQLite drops `main.i`, and gives `no such index: temp.i` for `temp.i`.
- An index name that is reserved, as `reserved index name`; see [Names](#names). For `drop index if exists sqlite_autoindex_t_1` SQLite gives the error that it gives without the clause.
- EXPLAIN before the statement, as `EXPLAIN`, and EXPLAIN QUERY PLAN, as `EXPLAIN QUERY PLAN`.

Syntax errors, which the pass gives as SQLite gives them: a second index, `drop index i, j`, which is `near ",": syntax error`; a table after the name, `drop index i on t`, which is `near "on": syntax error`; IF EXISTS after the name, `drop index i if exists`, which is `near "if": syntax error`; IF NOT EXISTS in place of IF EXISTS, which is `near "not": syntax error`; IF alone before the name, `drop index if i`, which is `near "i": syntax error`; and IF EXISTS twice, and an index named `exists` after the clause, which are `near "exists": syntax error`.

### Names

`table`, `column`, and `index` are names:

- A bare name, such as `t1` or `été`, or a keyword that SQLite reads as a name in that place, such as `abort`. The parser has decided which keywords are names, as SQLite's grammar does. The value of a bare name is its text.
- A name in double quotes, with `""` for a quote inside. Its value is the text between the quotes, with `""` read as `"`.

Not supported: the empty name `""`, in every place, though SQLite takes it for a name and creates a table and a column of that name. The storage core and the format of the commit object do not hold a table or a column without a name (`checkIdentifier` in `internal/format/check.go`), and the pass is the one gate that decides what is supported. It is the only name that SQLite and this pass accept and the core refuses: the core's other rules on names are valid UTF-8, no NUL, and no reserved prefix, which the checks before parsing and the reserved names cover already. `TestEngine_namesOfTheCore` in `internal/engine` checks, against the real core, that every name of the pools of the generator and thousands of random names in double quotes, of every kind of character, are taken by the core wherever the pass supports them, as SQLite takes them.

Not supported: a name in `[brackets]`, in backticks, or in single quotes, which SQLite accepts in some places. The roadmap's restrictions reject all three.

A table name is reserved, and makes any statement that names it not supported, if it is one of these, in any case of the ASCII letters:

- A name that starts with `sqlite_` or `s4ql_`. The roadmap's restrictions reject these, for every kind of statement, so `object name reserved for internal use` and `table sqlite_master may not be dropped` are not reproduced, and `sqlite_schema` cannot be read.
- The name of an **eponymous virtual table** of SQLite: `json_each`, `json_tree`, `jsonb_each`, `jsonb_tree`, and `pragma_` followed by the name of each PRAGMA that gives rows, such as `pragma_table_list`. SQLite gives every connection these tables without a CREATE. With a table of that name missing, `select * from json_each` gives rows in SQLite, `drop table json_each` gives `table json_each may not be dropped`, and `insert into json_each (key) values (1)` gives `table json_each may not be modified`; an engine that knows only its own tables would give `no such table`. So the pass rejects these names rather than let the engine give an answer that is not SQLite's. `pragma_x`, which names no PRAGMA, is a name like any other. The list holds 59 PRAGMAs. `TestReservedName` checks the list against SQLite: of every PRAGMA of the build, every module, and every module that SQLite registers when a statement names it, exactly those in the list can be read without a CREATE, in any case of the letters. The modules that SQLite registers when a statement names them are read from `sqlite3LocateTable` in the amalgamation by hand: a later version of SQLite that adds one there needs the test, and the list, extended.

A column name is never reserved: `sqlite_v` is a column name like any other.

An index name is reserved, and makes a CREATE INDEX or a DROP INDEX that names it not supported, as `reserved index name`, if it starts with `sqlite_` or `s4ql_`, in any case of the ASCII letters. `s4ql_` is reserved as it is for tables. For `sqlite_`, SQLite gives `object name reserved for internal use: sqlite_x` in CREATE INDEX, once it has found the table. For `drop index sqlite_autoindex_t_1` it gives `index associated with UNIQUE or PRIMARY KEY constraint cannot be dropped`: that is the index of the primary key of `t`, which every table of s4ql has. Neither message is reproduced.

The name of an eponymous virtual table, such as `json_each` or `pragma_table_list`, is an index name like any other. SQLite creates and drops such an index, and the virtual table works beside it.

The table after ON of a CREATE INDEX follows the rules of a table name: `create index i on sqlite_schema (a)` is `reserved table name`, and `create index i on json_each (a)` is `name of a virtual table of SQLite`.

### The statement and the text

- The statement may end with a semicolon. Before it, any number of empty statements, spaces, and comments may come, which SQLite skips. After it, only spaces, comments, and semicolons may come.
- EXPLAIN before a statement is not supported, as `EXPLAIN`, and EXPLAIN QUERY PLAN as `EXPLAIN QUERY PLAN`. The offset of both is that of the EXPLAIN.
- A text with a second statement is not supported, at the second statement's first token, even if the first statement is supported and the second is a syntax error.

### The errors that SQLite raises in grammar actions

The parser accepts every statement whose only fault is an error that one of SQLite's grammar actions raises, such as `unknown table option`. Its README lists them.

Two can occur for a statement that the pass supports, a CREATE TABLE of several columns:

- `duplicate column name: %s` needs a column with the name of an earlier column.
- `too many columns on %s` needs more than 2,000 columns.

SQLite gives `table %s already exists` in place of either when a table of the name exists, so the pass supports the statement and gives the error through `CreateTable.ColumnsError()`; see [CREATE TABLE](#create-table).

`too many SQL variables` needs more than 32,766 parameters, `SQLITE_MAX_VARIABLE_NUMBER`, which an insert of slice 5 can have, and a WHERE with subqueries of slice 15. SQLite gives it while it parses, at the 32,767th `?`, whatever the database holds. By the rule for such errors, the pass rejects the statement as not supported, as `parameter beyond SQLite's limit`, at that `?`. `TestCheck` checks that an insert of 32,766 parameters is supported and parses in SQLite, and that one of 32,767 is not supported where SQLite stops early.

`Expression tree is too large (maximum depth 1000)` needs an expression deeper than 1,000, `SQLITE_MAX_EXPR_DEPTH`, which the WHERE of slice 7 can be. SQLite gives an AND the depth of its deeper side plus one. A condition is 2 deep, and 3 deep with a sign before its value, on either side of its column. AND groups to the left, so each more condition makes the WHERE one deeper: 999 conditions are the most in a WHERE without a subquery, and 998 if one of the first two has a sign. SQLite gives the error while it parses, whatever the database holds. By the rule for such errors, the pass rejects the statement as not supported, as `WHERE deeper than SQLite's limit`, at the AND that makes it too deep (`maxDepth` and `depth` in `recognize.go`). `TestCheck` checks the limit at both edges against SQLite: the most conditions are supported and parse in SQLite, and one more is not supported where SQLite gives the error. The limit is the same for the WHERE of an update and of a delete, and a sign in SET does not count towards it. A subtest of `TestCheck` checks both edges in both statements, with a sign in the SET of the update.

A subquery of slice 15 counts towards the depth twice. The first count is while SQLite parses, whatever the database holds. A subquery is one deeper than its select, and the comparison one deeper than the subquery. A select is as deep as its deepest expression: its WHERE, its LIMIT, which is 2 deep, or a column, which is 1 deep. So `k = (select k from u where a = 1 and ...)` is too deep from 998 conditions in the subquery, where a WHERE of a statement takes 999. The pass rejects it as `WHERE deeper than SQLite's limit` where SQLite stops:

- With 998 conditions the comparison is too deep. The offset is that of the token after the comparison, or the end of the statement if no token is left.
- With 999 conditions the subquery is too deep. The offset is that of its closing parenthesis.
- From 1,000 conditions the WHERE of the subquery is too deep. The offset is that of the AND that makes it so, as in the WHERE of a statement.

The second count is when SQLite resolves the names, once it has found the tables. SQLite then adds the height of the WHERE of the statement and the height of the select of a subquery in it, and refuses more than 1,000 with the same message. So the WHERE of a subquery has 498 conditions at most, and 497 when AND joins its comparison to another condition. With one more, SQLite gives `Expression tree is too large (maximum depth 1000)` on a database with the tables, and `no such table` on an empty one. The pass does not know the database, and rejects the statement as `WHERE deeper than SQLite's limit`, at the opening parenthesis of the first subquery that makes the WHERE too deep (`recognizer.statementWhere`). This is the rule of `index beyond SQLite's limit`: an error that SQLite gives once it has found the table, and that the pass gives as not supported. The subtest "should take a subquery as deep as SQLite allows, and not one deeper" of `TestCheck` checks this limit at both edges against SQLite, on a database with the tables, for a subquery on the right of its column in a select, on the left in a delete, before an AND in an update, and after an AND in a select. The most conditions are supported, parse in SQLite, and run there. One more is not supported, at the subquery, and SQLite parses the text and gives the error for it on that database. The subtest also checks the three offsets of the first count.

Without a subquery, `parameter beyond SQLite's limit` cannot occur in a WHERE: the limit of the depth comes long before the 32,767th `?`. Nor can it occur for the `?` of a LIMIT, which comes after the 999 `?` of such a WHERE at most. With subqueries it can: a WHERE of many subqueries of few conditions each stays within the limit of the depth, and can hold more than 32,766 `?`. The pass counts the `?` of the whole statement in one counter (`recognizer.parameters`), and rejects the 32,767th wherever it is, also in a subquery and as the count of a LIMIT. The tests check the limit of the parameters at both edges for an insert only.

`too many columns in set list` needs more than 2,000 assignments, `SQLITE_MAX_COLUMN`, which the update of slice 8 can have. SQLite gives it while it parses, whatever the database holds. By the rule for such errors, the pass rejects the statement as not supported, as `SET beyond SQLite's limit`, at the 2,001st assignment. `TestCheck` checks the limit at both edges against SQLite: an update of 2,000 assignments is supported and parses in SQLite, and one of 2,001 is not supported where SQLite gives the error. So `parameter beyond SQLite's limit` cannot occur in an update without a subquery either: 2,000 assignments and 999 conditions hold 2,999 `?` at most, far below 32,766.

`too many columns in index` needs more than 2,000 columns, `SQLITE_MAX_COLUMN`, which the create index of slice 9 can have. SQLite gives it once it has found the table, before it looks for the columns, so also for 2,001 names that the table does not have. The pass rejects the statement as not supported, as `index beyond SQLite's limit`, at the 2,001st column. `TestCheck` checks the limit at both edges against SQLite. An index of 2,000 columns is supported and parses in SQLite, and SQLite creates it on a table of 2,000 columns. An index of 2,001 columns is not supported, and SQLite gives the error for it on that table.

`unsupported use of NULLS FIRST` and `unsupported use of NULLS LAST` need NULLS FIRST or NULLS LAST after a column of a create index. SQLite gives them on any database: it is the first check of `sqlite3CreateIndex`, before SQLite looks for the table. The pass rejects the statement as not supported, as `NULLS FIRST or NULLS LAST`, at the NULLS.

None of the others can occur for a supported statement:

- `unknown table option` needs an option other than `strict` or `without rowid`, as bare words.
- `object name reserved for internal use` needs a name that starts with `sqlite_`, which is reserved for a table and for an index.
- `unknown database` and `temporary table name must be unqualified` need a schema name or TEMP.
- `cannot create a TEMP index on non-TEMP table` needs a schema name on the name of an index, which is not supported.
- `the "." operator prohibited in index expressions` needs a column of an index with the name of its table, which is not supported.
- `default value of column [%s] is not constant` needs a DEFAULT in parentheses, such as `default (?)`, which is not supported.
- `index associated with UNIQUE or PRIMARY KEY constraint cannot be dropped` needs the name of an index that SQLite made itself, which starts with `sqlite_` and is reserved.
- `table "t" has more than one primary key` needs a second PRIMARY KEY, which is not supported.
- `unknown datatype for %s.%s: "%s"` needs a type that is not one of SQLite's standard ones in a STRICT table, which is not supported.
- A misplaced digit separator and a variable `#N` are syntax errors, which the parser gives.
- `variable number must be between ?1 and ?%d` needs a numbered parameter, such as `?0`, which is not supported.
- The rest need clauses, expressions, or commands that no supported form has.

So every statement with one of these other errors is not supported, and SQLite's message for it is not reproduced. A text in which an error of a grammar action comes before a syntax error is a case of its own: SQLite stops at the action's error, and the oracle does not say whether the text parses, while the parser goes on and gives the later syntax error. The pass then gives that syntax error, which is not SQLite's message; see [Known gaps](#known-gaps).

## Not supported

The message of the error is `s4ql: not supported: ` with the name of what is not supported and its offset, as in `s4ql: not supported: GROUP BY clause (at offset 16)` for `select * from t group by v order by v`. The offset is in bytes from the start of the text.

The rule for the offset: it is that of the first token, in the order of the text, that is not part of a supported statement. When a part that the statement needs is missing, such as PRIMARY KEY, the offset is that of the token where the part would go, or the end of the statement if no token is left.

The rule for the name, in this order:

1. The checks before parsing, the limits of the parser, and a second statement have names of their own.
2. Where the recognizer expects a token and finds one of another form, or finds that a part is missing, the name is that of the check, in the first table below.
3. Otherwise the token at the offset belongs to a construct that the recognizer did not accept. From the root of the tree, the walk goes down through the nodes that the recognizer accepted. If the token is one of such a node's own tokens, the name is that of its field, in the second table. If it is in a child that the recognizer did not accept, the name is that of the child's type, in the third table. So the name is that of the outermost construct around the token that is not supported: `values (1 + 2)` gives `binary operator`, not `literal`.

A test checks that every name the pass gives in any test is in these tables.

### Names of the checks

"The value" in this table is a value of an INSERT, of an assignment of UPDATE, or of a condition of WHERE, or the literal of a DEFAULT. The count of a LIMIT is not such a value, and the rows that cover it say so.

| Name | When |
| --- | --- |
| `NUL byte` | The text has a NUL byte. |
| `invalid UTF-8` | The text has a byte that is not valid UTF-8. |
| `text longer than SQLite's limit` | The text is longer than 1,000,000,000 bytes. The offset is that of the first byte past the limit. |
| `statement beyond a limit of the parser` | The statement is nested deeper than the parser reads, or is longer. The offset is that of the statement's first token. |
| `second statement` | A statement follows the first one. |
| `statement` | The recognizer expected a token after the end of the statement. No statement of SQLite's grammar gives this. |
| `name in brackets` | A name is in `[brackets]`. |
| `name in backticks` | A name is in backticks. |
| `single-quoted name` | A name is a string in single quotes. |
| `empty name` | A name is `""`. |
| `reserved table name` | A table name starts with `sqlite_` or `s4ql_`. |
| `name of a virtual table of SQLite` | A table name is that of an eponymous virtual table. |
| `bare name IF after IF NOT EXISTS` | CREATE TABLE IF NOT EXISTS or CREATE INDEX IF NOT EXISTS names the table or the index `if`, bare, in any case of its letters. SQLite keeps the text from the name on, `CREATE TABLE if (...`, which it cannot parse again: it gives `malformed database schema (if) - near "(": syntax error`, or near `"on"` for an index, and creates nothing. The offset is that of the name. |
| `reserved index name` | An index name starts with `sqlite_` or `s4ql_`. SQLite gives `object name reserved for internal use` for `sqlite_` in CREATE INDEX, and `index associated with UNIQUE or PRIMARY KEY constraint cannot be dropped` for the index of a primary key in DROP INDEX. |
| `table constraint` | CREATE TABLE has a table constraint. |
| `column without a type` | The column has no type. |
| `column type other than INT, INTEGER, REAL, TEXT, or BLOB` | The column's type is not one of the bare words `int`, `integer`, `real`, `text`, and `blob`. |
| `NULL constraint` | A column says NULL, without NOT before it. SQLite reads it as no constraint. |
| `second PRIMARY KEY` | A column says PRIMARY KEY after an earlier one did, or says it twice. SQLite gives `table "t" has more than one primary key`. |
| `second DEFAULT` | A column says DEFAULT after an earlier DEFAULT of the column. SQLite keeps the last one. The offset is that of the later DEFAULT. |
| `DEFAULT of another type than its column` | The literal of a DEFAULT is not of the type of its column, and not NULL: an integer for a column that is not INT or INTEGER, a real for one that is not REAL, a string for one that is not TEXT, or a blob for one that is not BLOB. SQLite takes the declaration, and converts the value or refuses it when an insert stores it; by the exact types of an insert, s4ql could never store it. The offset is that of the literal, its sign included. |
| `table without PRIMARY KEY` | No column says PRIMARY KEY. SQLite gives `PRIMARY KEY missing on table t` after it has parsed the statement. |
| `table option` | An option is not STRICT or WITHOUT ROWID, or comes again, or is a third. |
| `table without STRICT and WITHOUT ROWID` | CREATE TABLE has no options. |
| `table without STRICT` | CREATE TABLE has WITHOUT ROWID only. |
| `table without WITHOUT ROWID` | CREATE TABLE has STRICT only. |
| `REPLACE` | The statement is REPLACE INTO. |
| `column named twice` | The column list of INSERT, the SET of UPDATE, the columns of CREATE INDEX, or the ORDER BY of SELECT name a column that they named before, as SQLite compares names. SQLite takes the first value of INSERT for it, the last of SET, makes an index of the column twice, and orders by the first of ORDER BY. The offset is that of the later name. A name in brackets or backticks is `name in brackets` or `name in backticks` there, whatever the names before it: SQLite reads `[v]` as `v`, and `"[v]"` as `[v]`. |
| `SET beyond SQLite's limit` | The SET of UPDATE has more than 2,000 assignments, `SQLITE_MAX_COLUMN`. SQLite gives `too many columns in set list`. The offset is that of the 2,001st assignment. |
| `INSERT with SELECT` | INSERT has a SELECT in place of VALUES. |
| `+ sign` | The value has a `+` sign, but before NULL, or the count of LIMIT has one, whatever follows it. |
| `~ operator` | The value has a `~`, also after its `-`. |
| `NOT operator` | The value has a NOT, also after its `-`, or a condition of WHERE starts with NOT. |
| `second sign` | The value has a `+` or `-` after its `-`. |
| `hex literal` | The value or the count of LIMIT is a hex literal. |
| `number with a digit separator` | The value or the count of LIMIT has a digit separator. |
| `integer outside 64 bits` | The value or the count of LIMIT does not fit a signed 64-bit integer. |
| `infinite real number` | The value is a real number that SQLite reads as an infinity, such as `1e999`. |
| `string after a sign` | The value is a string after its `-`. |
| `blob after a sign` | The value is a blob after its `-`. |
| `TRUE or FALSE` | The value of INSERT or of an assignment of UPDATE, the literal of a DEFAULT, a result column of SELECT or of RETURNING, a column of CREATE INDEX, a condition of WHERE, its column, or its value, a column of ORDER BY, or the count of LIMIT is the bare word TRUE or FALSE, which SQLite reads as 1 or 0. As the column of an assignment it is a name. |
| `NULL after a sign` | The value is NULL after a `-` or a `+`, which SQLite reads as NULL. |
| `current date or time` | The value or the count of LIMIT is CURRENT_DATE, CURRENT_TIME, or CURRENT_TIMESTAMP. |
| `numbered parameter` | The value or the count of LIMIT is a parameter with a number, such as `?1`. |
| `named parameter` | The value or the count of LIMIT is a parameter with a name: `:name`, `@name`, `$name`, or `#name`. |
| `parameter beyond SQLite's limit` | The value or the count of LIMIT is the 32,767th parameter of the statement, also in a subquery. SQLite gives `too many SQL variables`. |
| `parameter after a sign` | The value is a parameter after its `-`. |
| `result column other than a column or *` | SELECT or RETURNING has a result column that is not the bare name of a column or `*`, such as `1`, `v + 1`, `t.v`, or `t.*`. |
| `* and another result column` | SELECT or RETURNING has `*` and another result column. |
| `SELECT without FROM` | SELECT has no FROM. |
| `value compared with a value` | A comparison of WHERE has a literal, a parameter, or a subquery on the left and one of the three on the right, also after a sign, as in `1 = 1`, `? = ?`, `'v' = 1`, or `? = (select k from u)`. It is at the value on the right. |
| `value before IS` | IS has a literal, a parameter, or a subquery on the left, also after a sign, as in `null is null`, `? is not null`, `5 is v`, or `(select k from u) is null`. |
| `column compared with a column` | A comparison of WHERE has a column on the right of a column, also in double quotes or with the name of its table, as in `v = w`. |
| `column with the name of a table` | The column of a condition of WHERE, a column of CREATE INDEX, or a column of ORDER BY has the name of its table before it, as in `t.v = 1`. SQLite gives `the "." operator prohibited in index expressions` for it in CREATE INDEX. |
| `OR` | WHERE has OR. The offset is that of the first OR, after the conditions before it. |
| `IS DISTINCT FROM` | A condition of WHERE is IS DISTINCT FROM or IS NOT DISTINCT FROM. The offset is that of DISTINCT. |
| `IS with a value other than NULL` | A condition of WHERE is IS or IS NOT with a literal other than NULL, a parameter, a name, a value after a sign, or a subquery, as in `v is 1`, `v is true`, or `v is (select k from u)`. The offset is that of the value. |
| `WHERE deeper than SQLite's limit` | The conditions of WHERE make an expression deeper than 1,000, `SQLITE_MAX_EXPR_DEPTH`, or the WHERE and the select of a subquery in it are together higher than 1,000. SQLite gives `Expression tree is too large (maximum depth 1000)`, for the first while it parses, and for the second when it resolves the names, once it has found the tables. The offset is that of the AND that makes it too deep, or of the closing parenthesis of a subquery whose select makes it too deep, or of the token after a comparison with a subquery that makes it too deep, or of the opening parenthesis of the first subquery whose select is too high together with the WHERE. |
| `DROP VIEW` | The statement is DROP VIEW. |
| `DROP TRIGGER` | The statement is DROP TRIGGER. |
| `index beyond SQLite's limit` | CREATE INDEX has more than 2,000 columns, `SQLITE_MAX_COLUMN`. SQLite gives `too many columns in index` once it has found the table. The offset is that of the 2,001st column. |
| `index on an expression` | A column of CREATE INDEX is an expression, such as `a + 1`, `lower(a)`, `-a`, a number, NULL, or a parameter. A string in single quotes is `single-quoted name`. The offset is that of the expression. |
| `partial index` | CREATE INDEX has a WHERE. The offset is that of the WHERE. |
| `EXPLAIN QUERY PLAN` | The statement starts with EXPLAIN QUERY PLAN. The offset is that of the EXPLAIN. |
| `ordering term other than a column` | A term of ORDER BY is a literal, a parameter, or an expression, such as `1`, `'a'`, `?`, `null`, `-a`, or `a + 1`. SQLite reads an integer literal that fits 32 bits as the position of a result column, and every other such term as an expression. A column in parentheses, with the name of its table, or with COLLATE has the name `parentheses`, `column with the name of a table`, or `COLLATE`. The offset is that of the term. |
| `LIMIT other than an integer` | The count of LIMIT is a real, a string, a blob, or NULL, such as `1.0`, `'5'`, or `null`. SQLite converts a real or a text that holds an integer to it when the select runs, and gives `datatype mismatch` for any other value. |
| `negative LIMIT` | The count of LIMIT has a `-` before it, whatever follows it, as in `-1`, which SQLite reads as no limit, `-0`, and `-?`. The offset is that of the `-`. |
| `LIMIT of an expression` | The count of LIMIT is an expression other than a literal, a parameter, a value after a sign, TRUE or FALSE, or parentheses, such as `1 + 1`, `~1`, or a name. |
| `OFFSET` | LIMIT has OFFSET, or a second number after a comma, `limit 10, 5`, whose first number is the offset. The offset of the error is that of the OFFSET or of the comma. |
| `subquery in a subquery` | A condition of the WHERE of a subquery has a subquery as its value, on either side of its column. The offset is that of the parenthesis of the inner subquery. |
| `subquery of more than one column` | The select of a subquery has a second result column. SQLite gives `row value misused` when it prepares the statement, once it has found the tables. The offset is that of the second column. A `*` before another column is `* and another result column`, at the `*`. |
| `subquery after a sign` | The value of a comparison is a subquery after a `-`. The offset is that of the parenthesis of the subquery. A `+` before a subquery is `+ sign`, at the `+`. |

### Names of fields

These are the tokens of a node that the recognizer accepted, in a field that no supported form has. Only the first token of such a construct can be at the offset, so only the fields that hold a first token are named.

| Name | Fields |
| --- | --- |
| `EXPLAIN` | `Statement.Explain`, for EXPLAIN without QUERY PLAN |
| `schema name` | `QualifiedName.Schema`, in any node |
| `TEMP` | `CreateTable.Temp` |
| `empty table option` | `CreateTable.OptionCommas`, for a comma before the first option |
| `CREATE TABLE AS` | `CreateTable.As` |
| `ASC or DESC` | `PrimaryKeyConstraint.Order` |
| `AUTOINCREMENT` | `PrimaryKeyConstraint.Autoincrement` |
| `DEFAULT of an expression` | `DefaultConstraint.LParen`, for a DEFAULT in parentheses, such as `default (1)` |
| `name after DEFAULT` | `DefaultConstraint.Name`, for a name after DEFAULT, bare or in quotes, which SQLite reads as a string |
| `UNIQUE INDEX` | `CreateIndex.Unique` |
| `NULLS FIRST or NULLS LAST` | `OrderingTerm.Nulls`, in a column of CREATE INDEX or of ORDER BY |
| `INSERT OR` | `Insert.Or` |
| `UPDATE OR` | `Update.Or` |
| `SET of a list of columns` | `Assignment.LParen`, for `set (v, w) = ...` |
| `DEFAULT VALUES` | `Insert.Default` |
| `alias` | `TargetTable.As`, `FromItem.As`, `FromItem.Alias`, `ResultColumn.As`, `ResultColumn.Alias` |
| `second row` | `Values.Commas` |
| `DISTINCT or ALL` | `SimpleSelect.Distinct` |

Any other field of an accepted node has the name of the node's type.

### Names of node types

| Name | Types |
| --- | --- |
| `literal` | `Literal` |
| `parameter` | `Variable` |
| `column reference` | `ColumnRef` |
| `parentheses` | `Paren` |
| `row value` | `Row` |
| `subquery` | `Subquery` |
| `EXISTS` | `Exists` |
| `unary operator` | `Unary` |
| `binary operator` | `Binary` |
| `IS` | `Is` |
| `NULL test` | `NullTest` |
| `LIKE, GLOB, REGEXP, or MATCH` | `Like` |
| `BETWEEN` | `Between` |
| `IN` | `InList`, `InSelect`, `InTable` |
| `COLLATE` | `Collate`, `CollateConstraint` |
| `CAST` | `Cast` |
| `function call` | `FuncCall` |
| `FILTER clause` | `Filter` |
| `OVER clause` | `Over` |
| `CASE` | `Case` |
| `WHEN` | `When` |
| `RAISE` | `Raise` |
| `SELECT` | `Select`, `SimpleSelect`, `SelectCmd` |
| `compound SELECT` | `Compound` |
| `VALUES` | `Values` |
| `row of VALUES` | `ValuesRow` |
| `result column` | `ResultColumn` |
| `FROM clause` | `From` |
| `join` | `FromItem`, `JoinOp` |
| `table` | `TableSource`, `TargetTable` |
| `table-valued function` | `TableFunction` |
| `subquery in FROM` | `SubquerySource` |
| `join in parentheses` | `NestedFrom` |
| `ON clause` | `JoinOn` |
| `USING clause` | `JoinUsing` |
| `INDEXED BY` | `IndexedBy`, with INDEXED BY |
| `NOT INDEXED` | `IndexedBy`, with NOT INDEXED |
| `WHERE clause` | `Where` |
| `GROUP BY clause` | `GroupBy` |
| `HAVING clause` | `Having` |
| `ORDER BY clause` | `OrderBy` |
| `ordering term` | `OrderingTerm` |
| `LIMIT clause` | `Limit` |
| `WITH clause` | `With` |
| `common table expression` | `CTE` |
| `column list` | `ColumnNames`, `IDList` |
| `column name` | `ColumnName` |
| `WINDOW clause` | `WindowClause` |
| `window definition` | `WindowDef` |
| `window` | `Window` |
| `window frame` | `Frame` |
| `frame bound` | `FrameBound` |
| `RETURNING clause` | `Returning` |
| `statement` | `Statement` |
| `BEGIN` | `Begin` |
| `COMMIT` | `Commit` |
| `ROLLBACK` | `Rollback` |
| `SAVEPOINT` | `Savepoint` |
| `RELEASE` | `Release` |
| `INSERT` | `Insert` |
| `ON CONFLICT clause` | `Upsert`, `OnConflict` |
| `UPDATE` | `Update` |
| `assignment` | `Assignment` |
| `DELETE` | `Delete` |
| `PRAGMA` | `Pragma` |
| `PRAGMA value` | `PragmaValue` |
| `VACUUM` | `Vacuum` |
| `ATTACH` | `Attach` |
| `DETACH` | `Detach` |
| `REINDEX` | `Reindex` |
| `ANALYZE` | `Analyze` |
| `CREATE TABLE` | `CreateTable` |
| `table option` | `TableOption` |
| `column` | `ColumnDef` |
| `column type other than INT, INTEGER, REAL, TEXT, or BLOB` | `TypeName` |
| `number` | `SignedNumber` |
| `CONSTRAINT name` | `ConstraintName` |
| `DEFAULT` | `DefaultConstraint` |
| `NULL or NOT NULL constraint` | `NullConstraint` |
| `PRIMARY KEY` | `PrimaryKeyConstraint` |
| `UNIQUE` | `UniqueConstraint` |
| `CHECK` | `CheckConstraint` |
| `REFERENCES` | `ReferencesConstraint`, `References` |
| `foreign key action` | `RefArg` |
| `DEFERRABLE` | `DeferConstraint`, `Deferrable` |
| `generated column` | `GeneratedConstraint` |
| `table constraint` | `TablePrimaryKey`, `TableUnique`, `TableForeignKey` |
| `DROP` | `Drop` |
| `CREATE VIEW` | `CreateView` |
| `CREATE INDEX` | `CreateIndex` |
| `CREATE TRIGGER` | `CreateTrigger` |
| `CREATE VIRTUAL TABLE` | `CreateVirtualTable` |
| `module argument` | `ModuleArg` |
| `ALTER TABLE` | `AlterTable` |
| `RENAME TO` | `RenameTable` |
| `ADD COLUMN` | `AddColumn` |
| `DROP COLUMN` | `DropColumn` |
| `RENAME COLUMN` | `RenameColumn` |
| `DROP CONSTRAINT` | `DropConstraint` |
| `ALTER COLUMN` | `AlterColumnNotNull` |
| `ADD CHECK` | `AddCheck` |

## Known gaps

- **An error that is not a syntax error, before a syntax error.** When one of SQLite's grammar actions raises an error, such as `unknown table option`, or SQLite's parser hits its stack limit, `Recursion limit`, and a syntax error comes later in the same statement, SQLite gives that error and the pass gives the syntax error. The oracle does not say on such a text, so no test can hold the pass to SQLite's answer there, and the promise covers only texts that are a syntax error in SQLite. Giving SQLite's message would need the pass to raise the action errors and to count the stack as SQLite does. A CREATE TABLE with a column named twice and a syntax error after that column is such a text: SQLite gives `duplicate column name`, and the pass gives the syntax error. In the tests, 36 entries of the corpus are such texts, and the tests count them apart.
- **An error about the database, before a syntax error.** Some grammar actions look the schema up while SQLite parses, so SQLite can give another error than the syntax error. The spec's rule settles which answer s4ql gives: "Text with a syntax error gets the error that SQLite gives for it on an empty database." So the answer of the pass never depends on what the database holds, and the engine gives it without a transaction.
  - **Covered by the rule:** SQLite starts a table, in `sqlite3StartTable`, as soon as it has read `CREATE TABLE`, the name, and the `(` or `AS` after it, or `CREATE VIRTUAL TABLE`, the name, `USING`, the module, and the `(`. If a table of the name exists, it gives `table %s already exists` with the name as written, and never meets a later syntax error: `create table t1 (v1 int primary key) strict without rowid` gives `table t1 already exists` when `t1` exists, and `near "without": syntax error` on an empty database, which is what the pass gives. This is the one difference from SQLite that the tests accept. `StartedTable` in [`stmttest`](stmttest/started.go), which is code for tests, gives the name of the table that such a text starts, and the engine's differential test and its fuzz tests accept exactly this difference with it. The engine's simulation does not need it: a statement that saw no state of the database, which a syntax error is, is compared with SQLite on an empty database. `TestStartedTable` and `FuzzStartedTable` check it against SQLite on hand-written texts, mutants and prefixes of statements of the generator, and any text. Not after TEMP, IF NOT EXISTS, or a schema other than `main`: SQLite then does not look in `main`, or gives `unknown database` first.
  - **A gap against the rule:** for the texts below SQLite gives another error than the syntax error also on an empty database, and the pass gives the syntax error. Each has the result code 1 and the extended code 1 on both sides; only the message differs. These are of the gap above, an error of a grammar action before a syntax error: `alter table nosuch add v1 int primary key x y` gives `no such table: nosuch`; `create table t2 (v1 int, v1 int) x y` gives `duplicate column name: v1`; others are `unknown database x`, `object name reserved for internal use: sqlite_x`, `temporary table name must be unqualified`, and `table "t2" has more than one primary key`; and `CREATE TRIGGER` gives `no such table: main.%s` at its `BEGIN` for a table that does not exist, before a syntax error in its body. On a database with the table and the column, the ALTER TABLE above gives `duplicate column name: v1`, and an INSTEAD OF trigger on a table gives `cannot create INSTEAD OF trigger on table: %s`: those differ from what SQLite gives on an empty database, so the rule does not ask for them, but the empty database's answer is still not the syntax error.

## How to add the next slice

1. Agree the slice in [docs/roadmap.md](../../../docs/roadmap.md).
2. Add a type for each new statement to `stmt.go`, or a field to an existing one, with what the engine needs to print SQLite's messages.
3. Extend the recognizer in `recognize.go`. Take every new token with `take`, accept every new node with `accept` before you take its tokens, and build the typed form only from tokens you took. Check the content of a token before you take it, with `isNext`, so that a reject names the right token. Do not loosen the check at the end of `recognize`.
4. Add a name to `fieldNames` in `vocabulary.go` for each field of an accepted node that the slice does not support, and every new name of a check, to the tables above.
5. Extend the allow-list in `vocabulary_test.go` with exactly the node types and fields that the slice adds. The test fails until every field outside the list has an example that is not supported.
6. Extend the generator to every new form, and `checkMeaning` in `stmt_test.go` to compare each new statement with what SQLite does. The generator in [`stmttest`](stmttest/stmttest.go) is shared with the tests of the engine and the driver. So put a new form behind an option of the generator, as `Generator.Columns` is for slice 5, `Generator.Nulls` for slice 6, `Generator.Conditions` for slice 7, `Generator.Changes` for slice 8, `Generator.Indexes` for slice 9, `Generator.Exists` for slice 10, `Generator.Defaults` for slice 11, `Generator.ValueFirst` for slice 12, `Generator.OrderBy` for slice 13, `Generator.Returning` for slice 14, and `Generator.Subquery` for slice 15, until the engine supports it. With the option off, the generator must write what it wrote before and draw the same random numbers, so that the generated tests of the engine and the driver draw the same calls. When the engine supports the form, it becomes an area of `stmttest.Sequence`, with a random stream and a profile of its own; [the README of `stmttest`](stmttest/README.md) has the steps.
7. Add hand-written cases for each new form and its near misses to `checkCases` in `check_test.go`.
8. Update this README: the definition of the slice, and the names.

## How it is tested

SQLite is the judge, through the oracle in [`internal/sql/oracle`](../oracle/README.md) and the binding in [`internal/sqlite`](../../sqlite/README.md).

`verify` in `check_test.go` checks one text, and every test layer below but the allow-list calls it:

- Text with a NUL byte or a byte that is not UTF-8 is not supported, with the right name and offset, and no other text is rejected for those.
- A syntax error is SQLite's, with its message and the codes 1 and 1. Where SQLite does not say, as in the [known gap](#known-gaps), the syntax error must still be one of SQLite's three messages, and the tests count it apart.
- A text that SQLite parses is never a syntax error.
- A "not supported" text is never a syntax error in SQLite. Where SQLite does not say whether it parses, the tests count it apart.
- A "not supported" error matches `ErrNotSupported`, has the form above, and has a name from the tables above, which the test reads from this README. Its offset is where a token that is not a space or a comment starts or ends.
- A text without a statement holds only spaces, comments, and semicolons.
- A supported statement is one that SQLite parses, and `checkMeaning` in `stmt_test.go` checks it in SQLite:
  - CREATE TABLE: the schema that SQLite records for the text is that of the canonical statement built from the typed form. `pragma_table_list` gives the name of the table, the number of its columns, `wr` 1, and `strict` 1. `pragma_table_xinfo` gives, for every column, its name, the name of its `Type`, `notnull` 1 for the primary key and for a column that says NOT NULL and 0 for a column that takes NULL, as `dflt_value` the text of `Default.Span` for a column with a DEFAULT and NULL for a column without one, and `pk` 1 for the primary key and 0 for every other column. The second run of the text gives `table <Raw> already exists`. The text and the canonical statement can spell one default in two ways, and `dflt_value` keeps the spelling. So `db.schema` compares a default by the value that SQLite gives its text with `select`, with its type.
  - CREATE TABLE with a DEFAULT, in `checkDefaults`, which `checkCreateTable` calls for a statement without a `ColumnsError()`: for each column with a DEFAULT, the value of `Default.Value` is exactly what SQLite gives for `select` and the text of `Default.Span`. Then an insert leaves that column out and gives every other column a value of its type, on the table of the text and on that of the canonical statement. For a table of one column it is `insert into t default values`. The insert stores the value of the DEFAULT, which the test reads back. A default of a real zero of either sign reads back as `0.0`. For DEFAULT NULL on a column that says NOT NULL or on the primary key, the insert gives `NOT NULL constraint failed: <table>.<column>`, with the extended code 1299. For a primary key with another DEFAULT, a second run of the insert gives `UNIQUE constraint failed: <table>.<column>`, with the extended code 1555.
  - CREATE TABLE with IF NOT EXISTS: the second run of the text does nothing, in place of `already exists`, and so does the text on a database that has a table of the name, with the case swapped, with another column. On a database with an index of the name, with the case swapped, the text gives `there is already an index named <Value>`. "Nothing" is checked by `db.nothing`: the text runs without an error, and the schema, as SQLite's PRAGMAs record it, and the text that `sqlite_schema` keeps of every object are the same before and after (`db.snapshot`).
  - CREATE TABLE with a `ColumnsError()`: on an empty database SQLite gives that error, with its message and the code 1, for the text and for the canonical statement. On a database that has the table, with the case of its name swapped, SQLite gives `table <Raw> already exists`, or does nothing for a text with IF NOT EXISTS.
  - INSERT, the values: the value of each literal is exactly what SQLite gives for `select` and the text of the literal, which the test reads from `Term.Span`. It has the same type and the same value: an integer by its number, a real by its 64 bits, and text and a blob byte by byte. The literal NULL is compared with what SQLite gives for `select null`, as every literal is.
  - INSERT, the table of the check: its name has the case of its ASCII letters swapped. Its columns are those of the list, with the case swapped, in the reverse order of the list, so that a value goes to its column by the name. Without a list, they are `c1` to `cn`, in the order of the values. The column of the first value that is not NULL is the primary key. Each column has the type of its value: INT for an integer and for a parameter, REAL, TEXT, or BLOB. The column of a NULL has one of the five types and no NOT NULL. Every other column that is not the primary key says NOT NULL. The parameters are bound to integers at or near the limits of 64 bits that differ from each other, so that a value in the wrong column is seen.
  - INSERT, what SQLite does: on that table, the text stores each value in its column, as the canonical statement does, and the test reads every column. A second run gives `UNIQUE constraint failed` with the names as the table stores them. Without the table the text gives `no such table: <Value>`. When the list and the values differ in number, it gives `N values for M columns`. Without a list, on a table with one more column, it gives `table <Value> has N+1 columns but N values were supplied`. With a list, on a table without the first column of the list, it gives `table <Value> has no column named <Value>`. The stored value is the value, but for a real zero: SQLite stores a real with an integer value as an integer in a REAL column, so `-0.0` reads back as `0.0`.
  - INSERT with a NULL, in `checkNotNull`: the table has the same columns in the same order, and every column says NOT NULL or is the primary key. If no value can be the key, the first column of the table is the key. SQLite gives `NOT NULL constraint failed: <table>.<column>`, with the extended code 1299, for the first column in the order of the table that gets a NULL, with the names as the table stores them. An insert of only NULLs has no column that can be the key, so no table stores its row. The check of the stored values is skipped for it, and every check of an error runs.
  - INSERT with RETURNING, the table of the check: it holds the columns of RETURNING too, since SQLite gives `no such column` for one that it lacks before it stores a value. With a list, the columns of RETURNING that the list does not name come after those of the list, once each, with the case swapped. Each is an INT column with a DEFAULT of its own, which the insert stores in it. Without a list, the table has as many columns as the statement has values. The first of them have the names of the columns of RETURNING, once each, with the case swapped, and the others are named by their place, `s4ql c1`, `s4ql c2`, and so on. When RETURNING names more columns than the statement has values, no such table holds them all: the checks of the stored values and of `checkNotNull` are skipped for the text, and every other check of an error runs.
  - INSERT with RETURNING, in `checkInsertReturning`: on that table, the text gives one row. Its columns are those of RETURNING, or every column for `*`, under the names as the table stores them, and its values are those that a select of the same columns reads after the insert. So the row has the values as the table stores them, and the DEFAULT of a column that the list leaves out. The canonical statement gives the same row.
  - INSERT with RETURNING, in `checkReturningMissing`, for a clause that names columns: the table has the columns of the list, or without a list as many columns as the statement has values, named `s4ql c1`, `s4ql c2`, and so on, and no other. SQLite finds the table, the columns of the list, and the number of the values. Then it gives `no such column` for the first column of RETURNING that the table does not have, or the message with the question about a string literal for a name in double quotes. A text whose list names every column of its RETURNING has no such column, and the check does not run for it.
  - SELECT without WHERE, ORDER BY, and LIMIT: the table of the check has the columns that the text names, once each, with the case swapped, and one more column. For `*` it has three columns, with the primary key second. It holds three rows, beside another table. The names of the result columns and the rows must be SQLite's: the names as the table stores them, and the rows in key order. The canonical statement gives the same. Without the table the text gives `no such table: <Value>`. On a table without the first name of the list it gives `no such column: <Value>`, or the message with the question about a string literal for a name in double quotes.
  - SELECT with WHERE, in `checkWhere`, the values: the value of each literal of a condition is exactly what SQLite gives for `select` and the text of the literal, as for a value of an insert. Each parameter is bound to a value of a type that the length of the text and the number of the parameter pick, NULL among them. The values differ from each other, so that a value that goes to the wrong condition is seen (`whereArgs`).
  - SELECT with WHERE, the table of the check: it has the columns that the text names, in the select list and in the conditions, once each, with the case swapped, and a primary key of its own, `s4ql key`, in the middle. The columns are of the type ANY, which SQLite compares with a value without converting either. Each column of a condition holds NULL, the least integer, the empty text, and the empty blob. For each of its comparisons it also holds the value of the comparison and the values of the same type just below and just above it (`around`). The rows are every combination of these values, or 100 at random, from a seed of the text, if the combinations are more (`whereRows`). They are inserted in the reverse of key order.
  - SELECT with WHERE, what SQLite does: on that table, the text and the canonical statement read the same rows, exactly, under the same names, which are the names as the table stores them. With `*` and without ORDER BY, the rows are in key order. Without the table the text gives `no such table: <Value>`. On a table without the first name of the select list it gives `no such column` for that name. Then the test takes the columns of the conditions that the select list does not name. On a table without the first of them, and on a table without all of them, SQLite gives `no such column` for the name that its rule picks, which [SELECT](#select) describes: a missing column under a test for NULL does not stop it. The canonical statement writes every condition with its column first. So for a text with a value on the left, the check compares the rows of the text with those of the condition turned round.
  - SELECT with ORDER BY or LIMIT, in `checkWhere` too, with a WHERE or without one, the table of the check: it has the columns of ORDER BY beside those of the select list and of the conditions. A column of ORDER BY that no condition tests holds the 14 values of `orderValues`: NULL and values of every type, of which the integer 2 and the real 2 compare equal, so that rows tie. With ORDER BY, the table has 30 rows at least. A `?` of LIMIT is bound to an integer that the length of the text picks: 0, 1, 3, 1,000, or the largest of 64 bits (`limitArg`).
  - SELECT with ORDER BY or LIMIT, what SQLite does: the text and the canonical statement read the same rows in the same order. Both would agree also if the pass read a direction wrong, so `checkOrder` checks the meaning against an order computed in Go. It reads the canonical statement with `*` in place of the select list. Its rows must be in the order of the columns of ORDER BY, each in its direction, as `compareValues` orders two values: NULL first, then the numbers, an integer and a real by their exact values, then the texts by their bytes, and then the blobs by their bytes. The columns are of the type ANY, so values of every type meet. With LIMIT, the rows must be as many as the count says, or as many as the statement without LIMIT reads if those are fewer. Each must have the values that the row at its place in the statement without LIMIT has in the columns of ORDER BY. Only those columns are compared, since rows that tie can come in another order. The rows of the text must be those rows, with the columns of the select list.
  - SELECT with ORDER BY, the errors: after the errors of the select list and of the conditions, the test takes the columns of ORDER BY that neither the select list nor a condition names. On a table without the first of them, and on a table without all of them, SQLite gives `no such column` for the first, or the message with the question about a string literal for a name in double quotes.
  - UPDATE and DELETE, in `checkChange`, the values: the value of each literal of an assignment and of a condition is exactly what SQLite gives for `select` and the text of the literal. Each parameter is bound as for a select with WHERE, to values that differ from each other, so that a value that goes to the wrong assignment or the wrong condition is seen.
  - UPDATE and DELETE, the table of the check: it is the table of a select with WHERE (`anyTable`), with the columns that the text names, those of the assignments first, once each, and the key `s4ql key` in the middle. Its rows are those of a select with WHERE for the conditions, and three at least. A column that no condition tests holds a number of its own in each row.
  - UPDATE and DELETE, what SQLite does: on that table, the text and the canonical statement leave the same rows, and `changes()` gives the same number after both. Each row that is left is the row that had its key before, or, for an update, that row with the values of the assignments. A delete leaves as many rows fewer as `changes()` says. An update leaves every row, and its `changes()` lies between the number of rows that differ from before and the number of rows that have the values of the assignments: a row that an update changes can keep its values. Without WHERE, `changes()` is the number of rows of the table. For a text with a value on the left, the canonical statement has the condition turned round, as for a select.
  - UPDATE and DELETE, the errors: without the table the text gives `no such table: <Value>`. An update on a table without the first column of its assignments gives `no such column: <Value>`. On a table without the last column of its assignments, and without the columns that only the conditions name, it gives `no such column` for that last column of the assignments. Both messages have the value of the name, also for a name in double quotes. Then the test takes the columns of the conditions that no assignment names, as it takes those of a select that the select list does not name: on a table without the first of them, and on a table without all of them, SQLite gives `no such column` for the name that its rule picks.
  - UPDATE and DELETE with RETURNING, in `checkChangeReturning`: the table of the check has the columns of RETURNING beside those of the assignments and of the conditions. Before the text runs, the test reads the keys of the rows that the conditions choose, with a select of `s4ql key`, since an update can change what its WHERE tests. The text gives a row for each of those rows, as many as `changes()` counts, with the columns of RETURNING, or every column for `*`, under the names as the table stores them. Those rows must be the rows that a select of the same columns reads for the chosen keys, after an update and before a delete. Both sets of rows are sorted before they are compared, so their order is not checked. The canonical statement gives the same rows as the text, in the same order. `returningReads` counts the texts with WHERE by whether they gave no row, some rows, or a row for every row of the table, and `TestGenerate` logs and requires the counts.
  - UPDATE and DELETE with RETURNING, the errors: after the errors of the assignments and of the conditions, the test takes the columns of RETURNING that neither an assignment nor a condition names. On a table without the first of them, and on a table without all of them, SQLite gives `no such column` for the first, or the message with the question about a string literal for a name in double quotes.
  - A select, an update, or a delete with a subquery, in `checkSubqueries`, which checks such a text in place of `checkWhere`, `checkChange`, and `checkChangeReturning`, the values: the value of each literal is exactly what SQLite gives for `select` and the text of the literal, in the assignments, in the conditions of the statement and of its subqueries, and in each LIMIT. Each parameter is bound as for a select with WHERE, and a `?` of a LIMIT, of the statement or of a subquery, as `limitArg` binds it.
  - With a subquery, the tables of the check: the statement has one table for each name of a table in it, by the fold of the name, so a subquery of the table of the statement reads that table. Each is a table as a select with WHERE has it (`anyTable`), under its name with the case swapped, with the columns that the statement names of it: for the table of the statement those of the select list, of SET, of the conditions, of ORDER BY, and of RETURNING, and for the table of a subquery its result column and those of its conditions and of its ORDER BY. Its rows are those of a select with WHERE for the conditions on it that have no subquery, and three at least. The column of a condition with a subquery and the result column of a subquery hold NULL, the least integer, the empty text, and the empty blob, where no other condition gives them values.
  - With a subquery, what SQLite does: the test computes the value of each subquery itself. It runs the select of the subquery alone, as the `Span` of the select holds it in the text, with the arguments of its own `?`, on the tables as they are before the text runs. The value is that of its column in its first row, or NULL without a row. Then the test writes the text with a `?` in place of each subquery, as its `Term.Span` holds it, and with the value as the argument of that `?`. On the tables, the text must do what that second text does, and so must the canonical statement. A select gives the same rows, under the same names. An update and a delete give the same rows of RETURNING, the same `changes()`, and leave the same rows in the table of the statement. So the parentheses, the span of each select, and the numbers of the parameters are those of the text, and the value of a subquery is that of its first row before the statement changes any. The rows of a result are sorted before they are compared, so this check does not see the order of a select with ORDER BY and a subquery. `subqueryReads` counts the subqueries by whether they gave no row, and so NULL, or a row, and `TestGenerate` logs and requires the counts.
  - With a subquery, the errors: without any table the text gives `no such table: <Value>` for the table of the statement, and without the table of a subquery, where that is another table, `no such table: <Value>` for it. A text with a subquery of `*` on a table of the check of more than one column gives `row value misused` in a select. In an update and a delete it gives `sub-select returns N columns - expected 1`, with the number of the columns of the table, but for `!=` or `<>` with the subquery on the left of its column, where it gives `row value misused` too. The check of such a text ends there.
  - DROP TABLE: the text drops the named table and leaves the other, as the canonical statement does; without the table it gives `no such table: <Value>`. With IF EXISTS it does nothing without the table, on an empty database and on a database where an index has the name, with the case swapped.
  - CREATE INDEX, in `checkCreateIndex`, the table of the check: its name is that of the table of the statement, with the case of its ASCII letters swapped. Its first column is a primary key of its own, `s4ql key`. After it comes a column for each column of the index, under its name with the case swapped, in the reverse order of the index.
  - CREATE INDEX, what SQLite does: on that table, the text makes the index that the canonical statement makes, and the test compares the schema that SQLite records for both (`schema()`). `sqlite_schema` has one index, of the name of the statement, on the table. `pragma_index_list`, without the index of the origin `pk`, which is the table itself, gives the name, `unique` 0, the origin `c`, and `partial` 0. `pragma_index_xinfo` gives each column of the index in the order of the text, under the name as the table stores it, with `desc` 1 if it says DESC and 0 if it does not, the collation `BINARY`, and `key` 1. After them it gives `s4ql key` with `key` 0.
  - CREATE INDEX, the errors: a second run of the text gives `index <Value> already exists`. So does the text on a database where another table has an index of the name with the case swapped. On a database with a table of the name of the index, with the case swapped, beside the table of the statement, the text gives `there is already a table named <Value>`. When the index has the name of its own table, SQLite gives that error always, with the columns and without them, for the text and for the canonical statement. Of the other checks, only that of the missing table then runs. Without the table the text gives `no such table: main.<Value>`, also when a table has the name of the index. On a table without the first column of the index, and on a table without all of its columns, it gives `no such column` for the first column, and on a table without the last column for the last, each with the question about a string literal for a name in double quotes. With IF NOT EXISTS, the text does nothing in the two places where it gives `index <Value> already exists` without the clause, and every other error of this check comes as without it.
  - DROP INDEX, in `checkDropIndex`: on a table with an index of the name, with the case swapped, and another index, the text drops the index of the name and leaves the other, as the canonical statement does. Without the index it gives `no such index: <Value>`, also when a table has the name. With IF EXISTS it does nothing in both places.
  - The canonical statement is supported and gives the same statement, the raw text of each name is at its offset, and the span ends where SQLite's prepare ends the statement. The canonical statement writes every name in double quotes, NOT NULL after PRIMARY KEY, DEFAULT after both, an integer in decimal, a real in Go's shortest form with an exponent (`strconv.FormatFloat(v, 'e', -1, 64)`), a string in single quotes, a blob in hex, and NULL as `NULL`. It writes NOT NULL only for a column that says it, and DEFAULT only for a column that has one, with its value as it writes every literal. It writes each condition of a WHERE as the name of its column, the operator as `Operator.String` gives it, and the literal or `?`, also a condition that says `ValueFirst`, and joins the conditions with ` and `. It writes a subquery as the canonical text of its select in parentheses, in the place of the literal. It writes each assignment of an update as the name of its column, ` = `, and the literal or `?`, and joins the assignments with `, `. It writes each column of a create index as its name, with ` desc` after a descending column and nothing after an ascending one, and joins the columns with `, `. It writes the columns of an ORDER BY in the same way, after ` order by `, and the count of a LIMIT as an integer in decimal or as `?`, after ` limit `. It writes the columns of a RETURNING as it writes a select list, each name in double quotes, joined with `, `, or `*`, after ` returning `, and nothing in a statement without the clause. It writes `if not exists` or `if exists` in a statement that says the clause, and nothing in one that does not.
  - For a select, an update, or a delete with WHERE, `checkPositions` checks that each condition comes after the table and after the condition before it, with its column first, or with its value first if it says `ValueFirst`, that the k-th `?` has the number k, and that a test for NULL has the zero `Term` and does not say `ValueFirst`. For a value that is a subquery it checks, in `checkTerm` and `checkSelectPositions`: the term has no value and no parameter, and names the next subquery of the statement, so that the conditions name every subquery of `Subqueries` once, in the order of the text; the span of the term starts at `(` and ends at `)`; the span of the select lies inside them, with only spaces and comments around it, and starts with SELECT; the select has one column or `*`, and no subqueries of its own; its conditions, its ORDER BY, and its LIMIT come in the places that those of a select statement have; and its `?` have the numbers after those before it in the text, as many as its `Parameters()` counts. For an update it checks the assignments too: there is at least one, each comes after the table and after the assignment before it, with its column first, no two have one column, and the `?` of the assignments have the numbers before those of the conditions, which come after the last assignment. For a select it checks the ORDER BY and the LIMIT too: the columns of ORDER BY come after the conditions, each after the one before it, there is at least one if `OrderBy` is not nil, and no two have one name; the LIMIT comes after them, its `?` has the number after those of the conditions, and a literal count is an integer from 0; and a select without LIMIT has the zero `Term`. For an insert, an update, and a delete it checks the RETURNING too, in `checkReturning`: a statement without the clause has the zero `Returning`; the span of a clause starts at the keyword RETURNING, after the values of an insert, the conditions of an update or a delete, or what comes before them, and ends at the end of its last column, or of its `*`, after which only spaces, comments, and the semicolon of the statement come; the columns come after the keyword, each after the one before it, and there is at least one if `Columns` is not nil; and `Parameters()` is the number of `?` that the check met before the RETURNING. For a create index it checks that the name of the index comes first, then the table, and then the columns, each after the one before it, that the columns are from 1 to 2,000, and that no two have one name.
  - For every statement, `Parameters()` is SQLite's count of its parameters, which the test reads as the largest index that `sqlite3_bind_int64` takes on the prepared statement.

The layers:

1. `TestCheck` pins the answer for each supported form and each near miss, with its exact message and offset, and checks each with `verify`. It also checks that every keyword that SQLite reads as a name after FROM is supported there. A second subtest checks the select list: a keyword is supported there as the name of a column if SQLite reads it as one, and no other keyword is. SQLite reads a keyword as the name of a column when it gives `no such column: kw` for it on a table without such a column. That holds for 84 keywords. Another subtest checks the same for the column of a condition of WHERE, where it holds for the same 84 keywords. Another checks the limit of the depth of a WHERE at both edges; see [the errors that SQLite raises in grammar actions](#the-errors-that-sqlite-raises-in-grammar-actions). Slice 8 added three subtests. The first checks the keywords in five new places: the table of an update, the table of a delete, the column of an assignment, and the column of a condition of an update and of a delete. A keyword is supported there if SQLite reads it as a name, which it does when it gives `no such table: kw` or `no such column: kw` for it, and no other keyword is. The pool of the generator for the place must hold exactly those keywords. The second checks the limit of 2,000 assignments at both edges, and the third the limit of the depth of a WHERE in an update and in a delete. Slice 9 added two subtests. The first checks the keywords in five places: the name of a new index, the table after ON, a column of an index, first in the list and after another column with DESC, and the name of a drop index. A keyword is supported there if SQLite reads it as a name, and no other keyword is. SQLite reads a keyword as a name when it gives `no such table: main.kw` for the table, `no such column: kw` for a column, and `no such index: kw` for a drop index. The name of a new index is a name when the text parses and SQLite gives `no such table` for the table after it. Each text runs on a database of its own, since SQLite makes an index on an expression of a keyword that it reads as a value, such as `null`. The pool of the generator for the place must hold exactly those keywords. The second checks the limit of 2,000 columns of an index at both edges. Slice 10 added 81 cases and two subtests. The cases are the supported forms of the two clauses, the forms that stay not supported after a clause, and the syntax errors of a clause in the wrong place, of half a clause, and of a clause twice. The first subtest checks the keywords as the name after each clause, in the four statements. A keyword is supported there if SQLite reads it as a name, and no other keyword is. SQLite reads a keyword as a name after IF NOT EXISTS when the statement creates a table or an index of that name, and after IF EXISTS when the statement drops the table or the index of that name, which the test created with the name in double quotes. The pool of the generator for the name of the plain statement must hold exactly those keywords, but for `if` after IF EXISTS, which is no name in the plain statement. That holds for 88 keywords after `create table if not exists` and after `create index if not exists`, and for 89 after `drop table if exists` and after `drop index if exists`. The second subtest pins what SQLite does for 41 texts with a clause, on a database that has the table `t` of the columns `v` and `w`, the index `i` on it, and one row. Each text runs on a database of its own, once as it is and once under `pragma query_only`, as a read-only transaction of s4ql runs it. The subtest pins SQLite's error, or that there is none, for both runs; that the schema and the text that `sqlite_schema` keeps are the same after the text, but for a text that creates or drops its object outside `query_only`; and that the count of changes stays 1, which the insert of the row set. It also pins the pass's answer for each text: supported, or the name of what is not supported. So it is the table of the facts that [CREATE TABLE](#create-table), [DROP TABLE](#drop-table), [CREATE INDEX](#create-index), and [DROP INDEX](#drop-index) give for the engine, and of the texts where SQLite does nothing and the pass says "not supported". Slice 11 added 97 cases and two subtests. The cases are 36 supported forms of DEFAULT, 48 forms that are not supported, and 13 syntax errors. They took the place of the two cases that refused every DEFAULT. The first subtest checks a create table as goqite's queue declares it, with IF NOT EXISTS and two INTEGER columns that say `not null default 0`. The second pins what SQLite does for 19 texts, which are 16 inserts, 2 updates, and a select, on a database of five tables with defaults, two of which have a row, with the length limit at 1,000 bytes. Each text runs on a database of its own, once as it is and once under `pragma query_only`. The subtest pins SQLite's error, or that there is none, for both runs, and the rows of the table of the text after the run without `query_only`. So it is the table of the facts about DEFAULT that [CREATE TABLE](#create-table) gives for the engine. Slice 12 added 75 cases and two subtests. The cases are 15 supported forms of a value on the left, 57 forms that are not supported, and 3 syntax errors. They took the place of the eight cases that refused a value on the left. Three older subtests grew. That of the keywords of the column of a condition checks them on the right of a literal and of a `?` too, where SQLite reads the same 84 as on the left. That of the keywords of an update and a delete checks two more places, `update t set x = 1 where 1 < kw` and `delete from t where ? >= kw`. That of the depth of a WHERE checks `1 = v` as the first condition, with 999 conditions at most, and `-1 = v`, with 998. The first new subtest, "should read a value on the left as SQLite does", runs 2,880 selects of the form `value op column` in SQLite, and requires of each the rows of `column op' value`, with `op'` the operator that `Operator.Swapped` gives. They are every operator in every spelling, with 20 values of every type, NULL and the limits of 64 bits among them, each as a literal and as a `?`, on a STRICT table with a column of each of the types INT, REAL, TEXT, BLOB, and ANY, and on a table that is not STRICT with a column of each of the first four. Of the 2,880 selects, 2,432 give some rows. The second, "should give SQLite's errors for a value on the left in the order of the other side", pins SQLite's error for 18 texts that the pass supports, which are 12 selects, 4 updates, and 2 deletes, on a table of three columns with one row, with the length limit at 1,000 bytes. So the two are the table of the facts about a value on the left that [SELECT](#select) gives for the engine. Slice 13 added 83 cases and four subtests, and four cases of a name in brackets or backticks after a name of the same fold in each list of columns. The cases are 8 supported forms of ORDER BY and LIMIT, 62 forms that are not supported, and 8 syntax errors. Six older cases have new answers: five that named `ORDER BY clause` or `LIMIT clause` are supported, and `limit null` is `LIMIT other than an integer`. The first subtest checks the keywords as a column of ORDER BY, in four places: alone, before DESC, after another column, and before ASC and a LIMIT after a WHERE. A keyword is supported there if SQLite reads it as the name of a column, and no other keyword is. SQLite reads the same 84 keywords as a column in each place, and they are the keywords of the pool of `SelectColumn`, which the generator draws the columns of ORDER BY from. The second, "should order a column of each type of a strict table as SQLite does", stores values in a column of each of the types INT, REAL, TEXT, and BLOB, with NULL in two rows, and reads them with `order by v` and with `order by v desc`. It requires the two NULLs first in ascending order and last in descending order, and the other values in the order of `compareValues`. It also pins the error with which each column refuses a value of another type. The third, "should limit as SQLite does", runs `select k from t limit ?` with 25 arguments, on a table of four rows and on an empty table. The arguments are integers, reals, texts, blobs, and NULL. For each it pins the number of rows, or `datatype mismatch` with the result code 20, on both tables. The fourth, "should give SQLite's errors for ORDER BY after those of the select list and of WHERE, in the order of the text", pins SQLite's error for 10 selects that the pass supports, on a table of two columns. So the three are the table of the facts about ORDER BY and LIMIT that [SELECT](#select) gives for the engine. Slice 14 added a section of 63 cases and two subtests. The cases are 16 supported forms of RETURNING, 33 forms that are not supported, and 14 syntax errors. Six of the 63 are older cases that have new answers: five that named `RETURNING clause` are supported, and `insert into t (v) values (?) returning ?` is `result column other than a column or *`, at the second `?`. The first subtest checks the keywords as a column of RETURNING, in four places: after an insert, after the SET of an update, after another column in a delete, and before another column after the WHERE of a delete. A keyword is supported there if SQLite reads it as the name of a column, and no other keyword is. SQLite reads the same 84 keywords as a column in each place, and they are the keywords of the pool of `SelectColumn`, which the generator draws the columns of RETURNING from. The second, "should give SQLite's errors for RETURNING after those of the rest of the statement, in the order of the text", pins SQLite's error for 20 texts that the pass supports, which are 11 inserts, 5 updates, and 4 deletes, on a table of two columns with two rows. For each it pins the message and the extended result code, that the text gives no row, and that the table holds its two rows after it. So it is the table of the facts about the errors of RETURNING that [RETURNING](#returning) gives for the engine. Slice 15 added a section of 66 cases and two subtests. The cases are 13 supported forms of a subquery, 48 forms that are not supported, and 5 syntax errors. One older case has a new answer: `select * from t where v = (select 1)`, which named `subquery`, is `result column other than a column or *`, at the `1`. The first subtest, "should take a subquery as deep as SQLite allows, and not one deeper", checks the limits of the depth of a subquery; see [the errors that SQLite raises in grammar actions](#the-errors-that-sqlite-raises-in-grammar-actions). The second, "should answer for a subquery what SQLite does with its errors, its value, and its LIMIT", pins what SQLite does for 35 texts that the pass supports, which are 23 selects, 8 updates, and 4 deletes. Each runs on a database of its own with three tables: `t` of two columns with three rows, or without rows for two of the texts, `u` of two columns with two rows, and `one` of one column with one row. For 21 texts it pins SQLite's error, and for the 14 others the rows that the text gives, with the arguments of its `?`, and for an update and a delete the rows that the text leaves in `t`. So it is the table of the facts that [Subqueries](#subqueries) gives for the engine.
2. `TestCheck_corpus`, `TestCheck_corpusMutants`, and `TestCheck_grammar` run `verify` on the corpus of [`internal/sql/corpus`](../corpus/README.md), on one mutant of each entry from `parsetest.Mutants`, and on sentences of SQLite's grammar from the generator of [`internal/sql/parsetest`](../parsetest/README.md) and one mutant of each.
3. `TestGenerate` runs the generator in [`stmttest`](stmttest/stmttest.go), from `generate_test.go`. Each statement must be supported as the statement that the generator wrote, offsets included, and pass `verify`. The check of a select, an update, or a delete with WHERE tells its conditions apart only on a table where they choose some rows and not others. So the test logs how many selects with WHERE read no row of the table of their check, some rows, and every row (`whereReads`), and how many updates and deletes with WHERE changed no row, some rows, and every row, as `changes()` counts them (`changeReads`), and how many updates and deletes with WHERE and RETURNING gave no row, some rows, and a row for every row (`returningReads`). It requires of each count that the statements with some rows are more than half of the others. With seed 0 and the default flags, 293 selects read no row, 456 some, and 0 every row, 561 updates and deletes changed no row, 922 some, and 0 every row, and of those with RETURNING 209 gave no row, 272 some, and 0 a row for every row. A statement with a subquery is not among these. `checkSubqueries` checks it, and counts each subquery that it runs by whether it gave no row or a row (`subqueryReads`). A subquery without a row is NULL, so the check tells the value of a subquery from NULL only where the subquery gives a row. The test logs both counts, and requires that some subqueries gave no row and that more gave a row. With seed 0, 383 subqueries gave no row and 1,152 a row. Of the 20,000 statements, 2,523 are updates, 2,483 are deletes, 2,550 are CREATE TABLE, 2,555 are DROP TABLE, 2,475 are CREATE INDEX, and 2,408 are DROP INDEX. A subquery is in 513 of the 2,493 selects, in 479 of the updates, and in 490 of the deletes.
   - The generator of `stmttest`, which the tests of the engine and the driver use too, writes random statements of the eight forms, an update and a delete only with `Generator.Changes`, a create index and a drop index only with `Generator.Indexes`, IF NOT EXISTS and IF EXISTS only with `Generator.Exists`, a DEFAULT only with `Generator.Defaults`, a value on the left of a comparison only with `Generator.ValueFirst`, an ORDER BY and a LIMIT only with `Generator.OrderBy`, a RETURNING only with `Generator.Returning`, and a subquery only with `Generator.Subquery`, with names from a pool of plain names, names in double quotes with odd content, names that differ only in case, names close to the reserved ones, and every keyword that SQLite reads as a name in that place; with each of the five types, in any case of its letters; with values of every type, with weight on their edges: integers at and around the limits, with leading zeros and gaps after the sign; reals at zero and -0.0, at the limits, subnormal, hard to round, in every form of a real literal, and in 17 to 40 digits; strings that are empty, hold `''`, look like SQL or a number, have letters of more than one byte, or are about 1,000 bytes long; and blobs that are empty, of zero bytes, or long, with hex digits in either case; in one insert in four a plain `?` in place of the value; with keywords in any case, and with spaces and comments in every gap.
   - `Generator.Columns` turns on the forms of slice 5. With 0, the generator writes what it wrote before slice 5 and draws the same random numbers, so the tests of the engine and the driver drew the same calls while they had it at 0. This was checked once, by hand: a dump of 27,000 texts from the code before slice 5 and the same dump from the code with slice 5 are the same, byte for byte. The tests of the pass set it to 3:
     - A create table has 1 to 3 columns, with the primary key at any position. The primary key says NOT NULL before PRIMARY KEY one time in three, after it one time in three, and not at all one time in three.
     - An insert has 1 to 3 values. One time in three it has no list of columns. Otherwise it has a list of as many names, which differ from each other.
     - A select has `*` one time in two. Otherwise it has 1 to 4 names from the pool of a position of its own, `select %v from t`, which holds 134 names. One name in four after the first repeats an earlier name.
     - Each column of a create table takes its name from the pool on its own, so two columns can have one name. Some create tables therefore have a `ColumnsError()`.
   - Three hooks give the forms of slice 5 in place of forms at random, when `Generator.Columns` is not 0. `Generator.Create` gives the columns of a create table: the name, the type, the key, and, for a column that is not the key, whether it is NOT NULL, in `stmt.Column.NotNull`. The generator writes NOT NULL after such a column only if the hook says so, and for the key as for columns at random. `Generator.Insert` gives the list of columns of an insert, or no list, and its values. `Generator.Select` gives the select list, or `*`. With the hooks nil, the generator draws exactly as before, so the pinned numbers of the tests of the pass did not move. `stmttest.Sequence`, which the tests of the engine and the driver use, sets `Columns` to 3 and the three hooks, so that each name of a table has a shape of its own, which its create tables, its inserts, and its selects share. [The README of the engine](../../engine/README.md#how-it-is-tested) has the shapes.
   - `Generator.Nulls` turns on the forms of slice 6. With 0, the generator writes what it wrote before slice 6 and draws the same random numbers, so the tests of the engine and the driver drew the same calls until the sequence wrote NULL. This was checked once, by hand: a dump of 27,000 texts and arguments of `Generator` and of `Sequence`, from 30 seeds, from the code before slice 6 and the same dump from the code with slice 6 are the same, byte for byte. `stmttest.Sequence` leaves it at 0, and writes NULL through its hooks since the generated checks of slice 6, which changed its calls. The tests of the pass set it to 5:
     - One column in five of a create table of slice 5 that is not the primary key has no NOT NULL, and takes NULL. This does not apply to the columns that the `Create` hook gives.
     - One value in five of an insert is NULL. It is written as `null`, `NULL`, `Null`, or `nULl`, or, where `Parameters` puts a `?` in its place, the argument of the `?` is nil. A value that the `Insert` hook gives is never replaced.
     - `Literal`, `Argument`, and `LiteralKind` know `stmt.Null`: `Argument` gives nil for it, and `LiteralKind` gives `"null"`.
   - `Generator.Conditions` turns on the forms of slice 7. With 0, and with the hook `Generator.Where` nil, the generator writes what it wrote before slice 7 and draws the same random numbers. This was checked once, by hand: a dump of 63,000 texts and arguments of `Generator`, with `Columns` at 0 and at 3 and `Nulls` at 0 and at 5, and of `Sequence`, from 30 seeds, from the code before slice 7 and the same dump from the code with slice 7 are the same, byte for byte. The tests of the pass set it to 4:
     - One select in two has a WHERE of 1 to 4 conditions, joined by AND.
     - The column of a condition comes from the pool of `SelectColumn`. One time in three it is the column of an earlier condition or of the select list.
     - The operator is one of the eight at random, so one condition in four is IS NULL or IS NOT NULL. `=` is written as `=` or `==`, and `!=` as `!=` or `<>`.
     - The value of a comparison is a value as an insert gets it, which is NULL one time in five. One time in four a `?` is in its place, with the value as its argument.
   - `Generator.Where` is the hook of slice 7. For a select of a table it gives the conditions of the WHERE, or none: the raw name of the column of each, its operator, and, for a comparison, its value as `Value.Value`. The generator writes an operator in any of its spellings, and a `?` in place of a value as for an insert. `TestGenerator_Where` in `stmttest` checks what it writes for the conditions that the hook gives. `stmttest.Sequence` leaves `Conditions` at 0, and sets `Where` since the generated checks of slice 7, which changed its calls. In its mixed profile, one select in two has a WHERE, whose values are mostly those that its inserts gave the columns, their neighbours, and the edges of each type. `stmttest.WhereCases` and `stmttest.KeyAccess`, in `rule.go`, name what such a select reaches and how the plan must read its rows, for the tests of the engine and the driver. [The README of the engine](../../engine/README.md#how-it-is-tested) has the forms and their rates.
   - `Generator.Changes` turns on the forms of slice 8. With 0, and with the hooks `Generator.Update` and `Generator.Delete` nil, the generator writes what it wrote before slice 8 and draws the same random numbers. This was checked once, by hand: a dump of 51,000 texts, statements, and arguments of `Generator`, with four settings of `Columns`, `Nulls`, and `Conditions`, and of `Sequence`, from 30 seeds, from the code before slice 8 and the same dump from the code with slice 8 are the same, byte for byte. `StatementOf` and `CallOf` write an update or a delete whatever `Changes` is. The tests of the pass set it to 3:
     - One statement in three of a kind at random is an update or a delete, each one time in two.
     - An update has 1 to 3 assignments, as many as `Columns` at most, to columns that differ from each other, from the pool of `UpdateColumn`. An assignment is written with `=`, or one time in four with `==`.
     - The value of an assignment is a value as a condition gets it, which is NULL one time in five. One time in four a `?` is in its place, with the value as its argument.
     - One update in two and one delete in two have a WHERE of 1 to 4 conditions, as a select has one. The column of a condition comes from the pool of `SelectColumn`. One time in three it is the column of an earlier condition or, in an update, of an assignment whose name SQLite reads as a column in WHERE too.
   - `Generator.Update` and `Generator.Delete` are the hooks of slice 8. For an update of a table, `Update` gives the assignments, one or more, each as the raw name of its column and its value as `Value.Value`, and the conditions of the WHERE, or none, as `Generator.Where` gives them. For a delete from a table, `Delete` gives the conditions, or none. The generator writes `=` or `==` in an assignment, an operator in any of its spellings, and a `?` in place of a value as for an insert, the `?` of the assignments before those of the conditions. `TestGenerator_Update`, `TestGenerator_Delete`, and `TestGenerator_Changes` in `stmttest` check what it writes. `stmttest.Sequence` leaves `Changes` at 0, and sets the two hooks since the generated checks of slice 8, when the engine ran an update and a delete.
   - The positions `UpdateTable`, `UpdateColumn`, and `DeleteTable` have pools of their own for slice 8, which hold exactly the names of the pools of `InsertTable` and `InsertColumn`. `Sequence` takes the names that its positions have in common from the new positions too, which changes no name. The kinds of the two statements are `KindUpdate` and `KindDelete`.
   - `Generator.Indexes` turns on the forms of slice 9. With 0, and with the hook `Generator.CreateIndex` nil, the generator writes what it wrote before slice 9 and draws the same random numbers. This was checked once, by hand: a dump of 54,900 lines of statements, calls, and arguments of `Generator`, in five settings of `Parameters`, `Columns`, `Nulls`, `Conditions`, and `Changes`, and of `Sequence`, from 30 seeds, from the code of `main` and the same dump from the code with slice 9 are the same, byte for byte. `StatementOf` and `CallOf` write a create index or a drop index whatever `Indexes` is. The tests of the pass set it to 4:
     - One statement in four of a kind at random is a create index or a drop index, each one time in two. The generator draws this after it has drawn for `Changes`, so an index statement takes the place of one statement in four of every other kind.
     - A create index has 1 to 3 columns, as many as `Columns` at most, with names that differ from each other, from the pool of `IndexColumn`.
     - A column is descending one time in three, and says DESC. Otherwise it says ASC one time in two, and neither one time in two.
   - `Generator.CreateIndex` is the hook of slice 9. For a create index of a name on a table, it gives the columns, one or more: the raw name of each, and whether it is descending. The generator writes ASC or nothing after a column that is not descending. `TestGenerator_CreateIndex` and `TestGenerator_Indexes` in `stmttest` check what the generator writes with the hook and with `Indexes`. `stmttest.Sequence` leaves `Indexes` at 0, and sets the hook since the generated checks of slice 9, when the engine ran a create index and a drop index. How often it writes the two statements is a weight of its profile.
   - The positions `IndexName`, `IndexTable`, `IndexColumn`, and `DropIndex` have pools of their own for slice 9: the name of a new index, the table after ON, a column of an index, and the name of a drop index. The pool of `IndexColumn` is checked as that of `SelectColumn` is: a keyword is in it only if SQLite gives `no such column` for it. The kinds of the two statements are `KindCreateIndex` and `KindDropIndex`.
   - `Generator.Exists` turns on the forms of slice 10. With 0, the generator writes what it wrote before slice 10 and draws the same random numbers. This was checked once, by hand: a dump of 64,440 lines of statements, calls, and arguments of `Generator`, in six settings of `Parameters`, `Columns`, `Nulls`, `Conditions`, `Changes`, and `Indexes`, and of `Sequence`, from 30 seeds, from the code before slice 10 and the same dump from the code with slice 10 are the same, byte for byte. The tests of the pass set it to 3:
     - One create table in three and one create index in three say IF NOT EXISTS, and one drop table in three and one drop index in three say IF EXISTS.
     - The keywords of the clause are in any case of their letters, with spaces and comments in the gaps between them and after them.
     - The name after the clause comes from the pool of the plain statement, so no statement of the generator has the bare name IF after IF NOT EXISTS, and none has `if` after IF EXISTS.
   - `TestGenerator_Exists` in `stmttest` checks that the generator writes the clauses in the four statements with `Exists`, and in none without it, and that the pass reads each statement as the generator wrote it. `stmttest.Sequence` sets `Exists` to the number of its profile, `Profile.Exists`, since the generated checks of slice 10, when the engine ran the clauses: 3 in the mixed profile, and 0 in a profile without them.
   - `Generator.Defaults` turns on the forms of slice 11. With 0, and with no column of the hook `Generator.Create` that has a `Default.Value`, the generator writes what it wrote before slice 11 and draws the same random numbers. This was checked once, by hand: a dump of 58,920 lines of statements and calls of `Generator`, in eight settings, and of `Sequence`, from 30 seeds, from the code before slice 11 and the same dump from the code with slice 11 are the same, byte for byte, once the zero `Default` that `%#v` prints for every column is stripped. The tests of the pass set it to 3:
     - One column in three of a create table says DEFAULT, also the primary key and a column that says NOT NULL. This does not apply to the columns that the `Create` hook gives.
     - The literal is a value of the type of its column, as an insert gets one, or NULL one time in `Generator.Nulls`, which is one time in five here. With `Nulls` at 0 it is NULL one time in eight.
     - The DEFAULT is at a place at random among the constraints of its column: before, between, or after PRIMARY KEY and NOT NULL.
   - A column that the `Create` hook gives says the DEFAULT of its `Default.Value`, whatever `Defaults` is, at a place at random too. `TestGenerator_Defaults` in `stmttest` checks that the generator writes no DEFAULT without `Defaults`, that it writes one of every kind of value, on the key, on a NOT NULL column, and before another constraint with it, that it writes the DEFAULT that the hook gives, and that the pass reads each statement as the generator wrote it. `stmttest.Sequence` leaves `Defaults` at 0, and its hook gives the DEFAULT of its shapes since the generated checks of slice 11, when the engine ran a DEFAULT, in a profile with defaults (`Profile.Defaults`).
   - `Generator.ValueFirst` turns on the forms of slice 12. With 0, and with no comparison of the hooks `Generator.Where`, `Generator.Update`, and `Generator.Delete` that says `ValueFirst`, the generator writes what it wrote before slice 12 and draws the same random numbers. This was checked once, by hand: a dump of 81,000 lines of statements and calls of `Generator`, in eight settings, three of them with hooks, and of `Sequence`, from 30 seeds, from the code before slice 12 and the same dump from the code with slice 12 are the same, byte for byte, once the `ValueFirst:false` that `%#v` prints for every condition is stripped. The tests of the pass set it to 3:
     - One comparison in three of a WHERE, of a select, an update, or a delete, is written with its value first, then the operator swapped, in any of its spellings, and then its column.
     - The condition that the generator gives for it reads as the comparison does with its column first, and says `ValueFirst`.
     - A test for NULL keeps its column on the left.
   - A comparison that a hook gives with `ValueFirst` set is written with its value first, whatever `Generator.ValueFirst` is. `TestGenerator_ValueFirst` in `stmttest` checks that the generator writes no value on the left without `ValueFirst`, that with it the generator writes one with every operator, also with a `?` as its value, in a select, an update, and a delete, and never in a test for NULL, that it writes the value first where the hook says so, and that the pass reads each statement as the generator wrote it. `stmttest.Sequence` sets `ValueFirst` to the number of its profile, `Profile.ValueFirst`, since the generated checks of slice 12, when the engine ran a value on the left: 3 in the mixed profile, and 0 in a profile without them. Its hooks give no such comparison.
   - `Generator.OrderBy` turns on the forms of slice 13. With 0, and with the hook `Generator.Order` nil, the generator writes what it wrote before slice 13 and draws the same random numbers. This was checked once, by hand: a dump of 73,800 lines of statements and calls of `Generator`, in six settings, three of them with hooks, and of `Sequence`, with `Sequence.Indexed`, which is `Profile.Indexes` of 2 since 2026-11-21, and without it, from 30 seeds, from the code before slice 13 and the same dump from the code with slice 13 are the same, byte for byte. The tests of the pass set it to 3:
     - One select in three has an ORDER BY of 1 to `Columns`+1 columns, which is 1 to 4 here, or of 1 or 2 without `Columns`.
     - A column of ORDER BY comes from the pool of `SelectColumn`. One time in three it is a column of the select list or of a condition. No two columns of one ORDER BY have one name.
     - A column is descending one time in three, and says DESC. Otherwise it says ASC one time in two, and neither one time in two.
     - One select in three has a LIMIT, drawn apart from the ORDER BY, so a select can have either, both, or neither.
     - The count is from 0 to 5 one time in two. Otherwise it is an integer as `stmttest.Integer` gives one, with its bits turned over if it is negative, so it is from 0 to the largest of 64 bits. One literal count in four has 1 to 30 leading zeros.
     - With `Parameters`, a `?` is in the place of the count one time in `Parameters`, which is one time in four here, with the count as its argument, after the arguments of the WHERE.
   - `Generator.Order` is the hook of slice 13. For a select of a table it gives the columns of the ORDER BY, each as the raw name of the column and whether it is descending, or none, and the LIMIT, with its count as `Value.Value`, or the zero `Term` for none. The generator writes what the hook gives, whatever `OrderBy` is: ASC or nothing after a column that is not descending, and the count as a literal or as a `?`. `TestGenerator_OrderBy` in `stmttest` checks that the generator writes no ORDER BY and no LIMIT without `OrderBy`; that with it the generator writes an ORDER BY of 1 to 4 columns, in each direction and with ASC, of columns that the select names before and of others, and a LIMIT that is small, large, with leading zeros, and a `?`; that it writes the ORDER BY and the LIMIT that the hook gives; and that the pass reads each statement as the generator wrote it. The tests of the engine and the driver set no `OrderBy`. `stmttest.Sequence` sets `Generator.Order` in the profiles that have the area `order` on, which [the README of `stmttest`](stmttest/README.md) describes.
   - `Generator.Returning` turns on the forms of slice 14. With 0, the generator writes what it wrote before slice 14 and draws the same random numbers. This was checked once, by hand: a dump of 32,400 lines of statements and calls of `Generator`, in three settings, one of them with hooks, and of `Sequence`, from 30 seeds, from the code before slice 14 and the same dump from the code with slice 14 are the same, byte for byte. The tests of the pass set it to 3:
     - One insert in three, one update in three, and one delete in three end with RETURNING.
     - The clause is `*` one time in three. Otherwise it has 1 to `Columns`+1 columns, which is 1 to 4 here, or 1 without `Columns`.
     - A column after the first is, one time in four, an earlier column of the clause, as it is written there, so the clause names it again.
     - Otherwise a column is, one time in two, a column that the statement names before: in the list of an insert, in SET, or in WHERE. Only a name that SQLite reads as a column of a select list is taken, since some keywords, such as `current_date`, are names after SET and values in a select list. A statement that names no such column draws nothing for this.
     - Otherwise a column is a name from the pool of `SelectColumn`.
   - `Generator.Return` is the hook of slice 14. For an insert, an update, or a delete of a table it gives whether the statement has a RETURNING, and the raw name of each of its columns, in their order and each as often as the clause names it, or none for `*`. The generator writes what the hook gives, whatever `Returning` is. `TestGenerator_Returning` in `stmttest` checks that the generator writes no RETURNING without `Returning`; that with it the generator writes `*` and lists of 1 to 4 columns, of columns that the statement names before and of others, and a column again; that without `Columns` it writes `*` or one column; and that the pass reads each statement as the generator wrote it. The tests of the engine and the driver set no `Returning`. `stmttest.Sequence` sets `Generator.Return` in the profiles that have the area `returning` on, which [the README of `stmttest`](stmttest/README.md) describes.
   - `Generator.Subquery` turns on the forms of slice 15. With 0, the generator writes what it wrote before slice 15 and draws the same random numbers. This was checked once, by hand: a dump of 10,320 lines of statements and calls of `Generator`, of every kind in three settings, one of them with hooks, and of `Sequence`, from 30 seeds, from the code before slice 15 and the same dump from the code with slice 15 are the same, byte for byte. The tests of the pass set it to 4:
     - The value of one comparison in four of a WHERE at random, of a select, an update, or a delete, is a subquery, on the side of its column where the value would be.
     - The select of the subquery reads the table of the statement one time in two, where the pool of `SelectTable` has the name of that table, and otherwise a table from that pool.
     - Its result column is `*` one time in eight, and otherwise a name from the pool of `SelectColumn`.
     - It has a WHERE, an ORDER BY, and a LIMIT as a select at random has them, and no subquery of its own.
     - Its `?` have their numbers and their arguments with those of the statement, in the order of the text.
   - A subquery has no hook, and a comparison that a hook gives is never replaced by one. `TestGenerator_Subquery` in `stmttest` checks that the generator writes no subquery without `Subquery`; that with it the generator writes statements with one subquery and with two, on either side of a column, of the table of the statement and of another, of `*` and of a column, with a WHERE and without one, and with an ORDER BY, a LIMIT, and a `?`; that no subquery has a subquery of its own; and that the pass reads each statement as the generator wrote it. `stmttest.Sequence` writes no subquery, and the tests of the engine and the driver set no `Subquery`, so they draw the same calls as before slice 15, until the engine runs a subquery.
   - A real comes from a float64 that `strconv` writes, in one of six forms: as `strconv` writes it, such as `1.5e+00`; without a point and with an upper-case `E`, such as `15E-1`; without an exponent; without an exponent and without a zero before the point, such as `.5`; with leading and trailing zeros; and with an upper-case `E` and the sign of the exponent, such as `1.5E+03`. Its expected value is the float64. That holds only if SQLite rounds those digits to the nearest float, which the 20,000 statements show for these texts.
   - A string comes from a pool and from random runes, and a blob from random bytes.
4. `TestGenerate_mutants` runs `verify` on five one-token mutants of each statement of the generator, and logs the mutants that are still supported, by how they differ from their statement. For a select with WHERE, `difference` names another column of a condition, another operator, another parameter, another kind of value, and another value, and since slice 12 a value on the other side of its column. Since slice 13 it names, for a select, another column of ORDER BY, another number of them, an ORDER BY where the statement has none or none where it has one, another direction of a column, a LIMIT where the statement has none or none where it has one, and, as the count of a LIMIT, another parameter, another kind of value, and another value. For an update it names the same for the assignments and for the conditions, and for a delete for the conditions. Since slice 14 it names, for an insert, an update, and a delete, a RETURNING where the statement has none or none where it has one, `*` in place of the columns of RETURNING or columns in place of `*`, another number of columns of RETURNING, and another name of one. Since slice 15 it names, for a select, an update, and a delete, a subquery where the statement has a literal or a `?` or one of those where it has a subquery, another number of subqueries, and another subquery, which is one whose select differs in any way that `difference` names for a select. For a create index it names another name of the index, another name of the table, another name of a column, another number of columns, and another order, and for a drop index another name of the index. For a create table it names another default of a column, and a default where the statement has none or none where it has one. `TestGenerate_nearMisses` adds what a one-token mutant cannot. It writes each statement of the generator in every form of its kind, 119 for CREATE TABLE, 97 for INSERT, 202 for SELECT, 78 for UPDATE, 54 for DELETE, 13 for DROP TABLE, 45 for CREATE INDEX, and 16 for DROP INDEX, and requires that none is supported. DROP TABLE had 8 forms before slice 9: `drop index` left them, since it is supported now. Slice 10 made four forms supported, one of each of the four kinds: the plain statement with IF NOT EXISTS or IF EXISTS. The 33 forms of slice 10 took their place. Slice 11 made three forms of CREATE TABLE supported: `default null` and `not null default 1` on a second INT column, and `default 1` on the primary key, which is supported when the key is INT or INTEGER. The 39 forms of slice 11 took their place, so CREATE TABLE had 118 forms then, where it had 82 before. Slice 12 made four forms supported: `where 1 = v` and `where ? = v` in a select, and `where 1 = v` in an update and in a delete. The 30 forms of slice 12 took their place, so SELECT had 110 forms then, where it had 92 before, UPDATE had 68, where it had 64, and DELETE had 45, where it had 41. Slice 13 made five forms of SELECT supported: `select v from t order by v`, `select v from t limit 1`, `select * from t limit 1`, `select * from t where v = 1 order by v`, and `select * from t where v = 1 limit 1`. The 54 forms of slice 13 took their place, so SELECT had 159 forms then, where it had 110 before. Slice 14 made six forms supported, two of each kind: `returning *` after an insert without a list and after one with a list, `returning *` after the SET of an update and a column of RETURNING after its WHERE, and `returning *` after a delete without a WHERE and after one with a WHERE. The 30 forms of slice 14 took their place, so INSERT had 95 forms then, where it had 80 before, UPDATE had 73, where it had 68, and DELETE had 49, where it had 45. Slice 15 made no form supported, and added 56 forms. So CREATE TABLE has 119 forms, where it had 118, INSERT has 97, where it had 95, SELECT has 202, where it had 159, UPDATE has 78, where it had 73, and DELETE has 54, where it had 49.
   - Most forms add or change a clause, a keyword, or an item, such as GROUP BY, RETURNING, a column constraint after PRIMARY KEY, or a second row.
   - Forms of the columns: a table without a primary key whose second column takes NULL, a second PRIMARY KEY, NOT NULL twice, a NULL constraint on the primary key, on a column before it, and on a column after it, another constraint after NOT NULL, a table constraint after the columns, a column and `*` in one select list, and an alias of a column.
   - Forms of the type: a type in double quotes, single quotes, or brackets, `any`, `varchar`, `double`, a type twice, and a plural such as `texts`. The forms of CREATE TABLE use the type of the generated statement.
   - Forms of the literal: an unterminated string, `'a''`, two strings without a comma, `x'6'`, `x'6g'`, `x '61'`, `1e`, `1e+`, `1.5.5`, `.`, `1e999`, `-1e999`, `1_000.5`, `0x1p3`, TRUE, `- false`, `"a"`, a cast, `- 'a'`, `-x'01'`, a concatenation, a literal followed by a letter or a quote, and two literals without a comma.
   - Forms of NULL: `+null`, `-null`, `- null` as a second value, `nullif(1, 1)`, `cast(null as int)`, `is null`, `is not null`, `isnull`, `notnull`, and `not null` after the value, `select null from t`, and `isnull` and `not null` in WHERE. Slice 6 made four forms supported: a column without a constraint after the primary key and one before it, and NULL as the only value and as a second value. Each was replaced by a form that is still not supported. Slice 7 made two more forms supported, `is null` and `is not null` in WHERE, and `isnull` and `not null` replaced them.
   - Forms of WHERE, which slice 7 added: a column on the right, the name of the table before the column, OR, OR after AND, NOT, parentheses around the condition or around the value, an expression, a function, IN, BETWEEN, LIKE, GLOB, `is 1`, `is true`, `is not false`, IS DISTINCT FROM and IS NOT DISTINCT FROM, COLLATE on either side, `+1`, `-?`, `?1`, and `true` as the value, AND at the end, a column alone, and RETURNING after the WHERE. Slice 8 made two forms supported, a WHERE in UPDATE and in DELETE, and the form with RETURNING took their place. Slice 12 made two more forms supported, a literal and a `?` on the left, and forms of slice 12 took their place. Slice 13 made two more supported, ORDER BY and LIMIT after the WHERE, and forms of slice 13 took their place.
   - Forms of UPDATE, which slice 8 added: EXPLAIN and EXPLAIN QUERY PLAN, WITH, each of the five UPDATE OR, the schema `main` or `temp`, an alias, INDEXED BY, NOT INDEXED, a column twice in SET, a list of columns in parentheses, FROM, LIMIT, and ORDER BY, the last two also after a WHERE. The two forms with RETURNING, after SET and after a WHERE, are supported since slice 14. As the value of an assignment: an expression, parentheses, a `+` sign, a column, also with its table or as `"a"`, `?1`, `:a`, `-?`, `true`, COLLATE, a cast, a concatenation, `is null`, a function, a subquery, and DEFAULT. In SET: `<>` in place of `=`, the table before the column, a column in brackets or in single quotes, a comma at its start or at its end, no assignment, and no SET. In WHERE: nothing after it, a column or a literal alone, OR, NOT, the table before the column, `isnull`, `is 1`, IN, a column on the right, and AND at the end. The form with a literal on the left is supported since slice 12.
   - Forms of DELETE, which slice 8 added: EXPLAIN and EXPLAIN QUERY PLAN, WITH, the schema `main` or `temp`, an alias, INDEXED BY, NOT INDEXED, LIMIT, and ORDER BY, the last two also after a WHERE, no FROM, a second table, and a table in parentheses. The two forms with RETURNING, without a WHERE and after one, are supported since slice 14. In WHERE: nothing after it, a column or a literal alone, OR, NOT, parentheses, an expression, `+1`, `?1`, `-?`, and `true` as the value, IN, BETWEEN, LIKE, a column on the right, the table before the column, `is 1`, IS DISTINCT FROM, `notnull`, COLLATE, AND at the end, and a comma between two conditions. The form with a literal on the left is supported since slice 12.
   - Forms of CREATE INDEX, which slice 9 added: EXPLAIN and EXPLAIN QUERY PLAN, UNIQUE, TEMP, the schema `main` or `temp` on the index, the schema `main` on the table, a WHERE with a comparison and with `is not null`, COLLATE, also before DESC, NULLS FIRST, and NULLS LAST after DESC. As a column: a sum, a negated column, a function, the column in parentheses, the column with its table, also after `main`, the column twice, also with ASC and with DESC, the column in single quotes or in brackets, `1`, `null`, `true`, `?`, and `current_time`. In the list: no column, a comma at its end, and ASC and DESC after one column. Also no list, DESC after the list, no ON, no name of the index, and a second statement.
   - Forms of DROP INDEX, which slice 9 added: EXPLAIN and EXPLAIN QUERY PLAN, the schema `main` or `temp`, ON and a table after the name, a second name after a comma, no name, REINDEX and ANALYZE of the name, and a second statement.
   - Forms of IF NOT EXISTS and IF EXISTS, which slice 10 added. In each of the four kinds: the clause in the wrong place, which is before TABLE or INDEX in a create and after the name in a drop; the other clause, IF EXISTS in a create and IF NOT EXISTS in a drop; the clause twice; and the clause before a name with the schema `main`. In each kind but CREATE INDEX, IF alone. In a create, IF NOT without EXISTS, and the bare name IF after the clause. In CREATE TABLE, the clause with TEMP, in a table with STRICT alone, in a table with a second PRIMARY KEY, and before AS SELECT. In CREATE INDEX, the clause with UNIQUE, in a partial index, and after ON. In DROP TABLE, DROP VIEW with the clause. In a drop, EXPLAIN before the statement with the clause.
   - Forms of DEFAULT, which slice 11 added, all in CREATE TABLE. On the primary key: `+1`, `+null`, `-null`, `- -1`, `true`, `false` before NOT NULL, `0x1`, `1_0`, `9223372036854775808`, `1e999`, `-'a'`, `-x'01'`, `"a"`, `a`, `[a]`, `current_time`, `?`, `(null)`, `(?)`, no literal, two strings, `default null` twice, also with PRIMARY KEY between the two, and a NULL constraint or COLLATE after `default null`. On a second column: a literal of another type than the column, in nine pairs of a type and a literal, `default 1 default 2`, `(1 + 1)`, `current_timestamp`, and UNIQUE after the DEFAULT. And `default +1` on the primary key after IF NOT EXISTS. The form `default (1)` on the primary key is older than slice 11.
   - Forms of a value on the left, which slice 12 added. In a select, 20 forms. A value on both sides: `1 = 1`, `? = ?`, and `1 = ?`. A value before IS: `null is null`, `? is not null`, and `1 is` before a column. As the value on the left: the value in parentheses, `+1`, `-?`, `?1`, `true`, and `0x1`. As the column on the right: a sum, the column in parentheses, the column with its table, and the column with COLLATE. Also `1 < v < 2`, OR between two such comparisons, AND at the end, and nothing after the operator. In an update, 5 forms: `1 = 1`, `1 is null`, the value in parentheses, the column with its table, and OR. In a delete, 5 forms: `1 = 1`, `1 = ?`, `? is null`, a `-` before the column on the right, and OR.
   - Forms of ORDER BY and LIMIT, which slice 13 added, 54 forms, all in a select. Of ORDER BY, 25 forms. As a column: `1` after a select list of one column, `0`, `-1`, a sum, the column in parentheses, a negated column, the column with its table, the column with COLLATE, the column in single quotes or in brackets, `?`, `null`, and `true`. After a column: ASC and DESC, NULLS FIRST, and NULLS LAST after DESC. In the list: the column twice, also with ASC and with DESC, no column, and a comma at its end. Also ORDER without BY, ORDER BY twice, and ORDER BY after GROUP BY, after a compound, and after a WHERE with OR. Of LIMIT, 29 forms. As the count: none, also after an ORDER BY, `-1`, `-0`, `+1`, `1.5`, `1.0`, `1e2`, `'5'`, `x'05'`, `null`, `0x5`, `9223372036854775808`, `1_0`, `current_time`, `true`, the column, `1 + 1`, `(1)`, `?1`, `:a`, and `-?`. With an offset: `limit 1, 1`, `offset 1` without LIMIT, and `limit 1 offset 0` after an ORDER BY. Also LIMIT before ORDER BY, LIMIT twice, NULLS FIRST in an ORDER BY between a WHERE and a LIMIT, and a second statement. The forms `order by 1` and `limit 1 offset 1` after `*` are older than slice 13.
   - Forms of RETURNING, which slice 14 added, 30 forms. In an insert, 17 forms. As a column: a sum, the column with AS and an alias and with an alias alone, the column with its table, `*` with the table, the value of the insert, and `?`. With `*`: `*, *`, `*` before the column, and `*` after it. In the list: no column, and a comma at its end. Also RETURNING twice, and `returning *` after a second row, after an upsert, after DEFAULT VALUES, and before a second statement. In an update, 7 forms: the column with AS and an alias, in a function, in parentheses, and with its table, `*` after the column, RETURNING before the WHERE, and `returning *` after UPDATE OR REPLACE. In a delete, 6 forms: `*` with the table, the column with COLLATE, the column in parentheses, the literal `1`, RETURNING before the WHERE, and no column. The form `returning *` after the WHERE of a select is older than slice 14.
   - Forms of a subquery, which slice 15 added, 56 forms. In a select, 43 forms. In the select of the subquery: a subquery in its WHERE, on the right and on the left of a column; two columns, a column and `*`, and `*` and a column; a sum, `count(*)`, an alias, and DISTINCT; UNION, and UNION ALL with a select without FROM; GROUP BY, OFFSET, a column with its table, OR, and `?1`; a subquery in its FROM, a second table, the schema `main`, and WITH; and VALUES in its place, no FROM, and the literal `1` as its column. Around the subquery: parentheses, a `-`, a `+`, COLLATE, `= 1` after it, and OR after it. In place of a comparison of a column with it: IN, NOT IN, EXISTS, the subquery alone, `is null` after it, a `?` or a subquery on the other side, and IS before it. In another place: the select list, ORDER BY, and LIMIT. Also no closing parenthesis, one too many, and empty parentheses. In an update, 5 forms: a subquery of a column in SET, a subquery in the WHERE of a subquery, a subquery of two columns, IN, and a subquery in RETURNING. In a delete, 5 forms: a subquery on the left in the WHERE of a subquery, a compound in a subquery of `*`, a subquery after a `-`, EXISTS, and a subquery in RETURNING. In an insert, 2 forms: a subquery as the value, and one in RETURNING. In a create table, 1 form: `default ((select 1))` on the primary key. The form `set v = (select 1)` of an update is older than slice 15.
5. `TestCheck_reals` in `real_test.go` checks the value of real literals against SQLite, on texts made to be hard to round: any digits with a point anywhere; a float's exact value halfway to the next one, cut to 15 to 40 significant digits, and one more or one less in its last digit; runs of nines and zeros; texts around the largest float and the smallest ones; leading zeros; and exponents far out of range or of many digits. One text in three comes after a `-`. For each, the pass gives the value that SQLite's `select` gives, bit for bit, or rejects a literal that SQLite reads as an infinity. The test logs for how many texts Go's `strconv.ParseFloat` gives another float than SQLite.
6. `TestAllowList` in `vocabulary_test.go` parses many statements, and requires that a supported statement uses only the node types and fields of the supported forms. Slice 4 added none: its types and its literals are in nodes and fields that slice 1 has. Slice 5 added `NullConstraint` with `Not` and `Null`, `ColumnRef` with `Column`, `ResultColumn.Expr`, and the commas `CreateTable.Commas`, `IDList.Commas`, `ValuesRow.Commas`, and `SimpleSelect.Commas`. Slice 6 added none: the literal NULL is a `Literal`, a column that takes NULL has no constraint, and the NULL constraint, which is not supported, is a `NullConstraint` without `Not`. Slice 7 added `SimpleSelect.Where`, `Where` with `Where` and `X`, `Binary` with `X`, `Op`, and `Y`, and `Is` with `X`, `Is`, `Not`, and `Y`. Slice 8 added `Update` with `Update`, `Table`, `Set`, `Assignments`, `Commas`, and `Where`, `Assignment` with `Column`, `Eq`, and `X`, and `Delete` with `Delete`, `From`, `Table`, and `Where`. Slice 9 added `CreateIndex` with `Create`, `Index`, `Name.Name`, `On`, `Table`, `LParen`, `Columns`, `Commas`, and `RParen`, and `OrderingTerm` with `X` and `Order`. Slice 10 added `CreateTable.IfNotExists` and `CreateIndex.IfNotExists`, each with `If`, `Not`, and `Exists`, and `Drop.IfExists` with `If` and `Exists`. Slice 11 added `DefaultConstraint` with `Default`, `Sign`, and `X`. Slice 12 added none: a value on the left of a comparison is in `Binary.X`, in the node types that hold a value on the right. Slice 13 added `SimpleSelect.OrderBy`, `OrderBy` with `Order`, `By`, `Terms`, and `Commas`, `SimpleSelect.Limit`, and `Limit` with `Limit` and `X`. A column of ORDER BY is an `OrderingTerm` with `X` and `Order`, which slice 9 has. Slice 14 added `Insert.Returning`, `Update.Returning`, `Delete.Returning`, and `Returning` with `Returning`, `Columns`, and `Commas`. A column of RETURNING is a `ResultColumn`, as a column of a select list is. Slice 15 added `Subquery` with `LParen`, `Select`, and `RParen`. The select of a subquery is a `Select` with a `SimpleSelect`, as that of a select statement is. It reads the node types from the source of `internal/sql/ast` and their fields through reflection, so the list cannot go stale: every node type, and every field of the node types of the supported forms, must occur in some statement of the test, so that a statement that has one outside them is seen to be not supported. The test sees types and fields, not how many there are or what their tokens say: `strict, without rowid, strict` and `values (+1)` use only the types and fields of the supported forms. The hand-written cases and the check of the meaning cover those. `vocabulary_internal_test.go` checks that every node type has a name, and `TestRecognize` there gives the recognizer trees that no text gives, for its paths that SQLite's grammar never reaches.
7. `FuzzCheck` runs `verify` on any text, and `FuzzGenerate` on a statement of the generator and a mutant of it. The seeds of `FuzzCheck` include the hand-written cases, those of slices 9, 10, 11, 12, 13, 14, and 15 among them. `FuzzLiteral` takes any text as the value of an insert and, if the pass supports it, compares its value with what SQLite's `select` gives. It also takes the text as the value of `select * from t where v = <text>`, and compares the value of each literal of the conditions with SQLite's. A text with AND is then several conditions. Since slice 8 it takes the text as the value of `update t set v = <text>` and of `delete from t where v = <text>` too, and compares the value of each literal of the assignments and of the conditions with SQLite's. A text with commas is then several assignments. Since slice 12 it takes the text as the value on the left too, in `select * from t where <text> < v`, and compares the value of each literal of the conditions with SQLite's. Since slice 13 it takes the text as the number of `select * from t limit <text>`, where the pass must give a `?` or an integer from 0, with the value that SQLite gives the text. One of its seeds is NULL, and its seeds include the values of the hand-written cases of the WHERE of a select and of SET. `make fuzz` runs all three.
8. [`internal/sql/mutation`](../mutation/README.md) runs mutation testing of this package.

Under `-short`, the race detector, or the address sanitizer, the tests on the corpus, the grammar, the generator, and the real literals use a tenth of their texts.

## Results

On the code as it is now, with the default flags:

| Layer | Texts | Rejected before parsing | Syntax error | Syntax error where SQLite does not say | Not supported | Not supported where SQLite does not say | No statement | Supported |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Corpus, and the parts before a byte rejected before parsing | 183,694 | 44,460 | 52,938 | 62 | 59,204 | 237 | 6,437 | 20,356 |
| One mutant of each corpus entry, and the parts before such a byte | 168,292 | 29,535 | 120,774 | 78 | 11,161 | 178 | 5,995 | 571 |
| 20,000 sentences of the grammar and a mutant of each | 40,000 | 0 | 12,996 | 2,356 | 20,189 | 1,479 | 2,895 | 85 |
| Five mutants of each of 20,000 statements of the generator | 100,000 | 0 | 83,137 | 35 | 6,263 | 139 | 0 | 10,426 |
| Near misses of 2,000 statements of the generator | 153,278 | 0 | 24,965 | 0 | 126,038 | 2,275 | 0 | 0 |

The two columns "where SQLite does not say" are the texts that the oracle excuses from the comparison with SQLite: SQLite stopped before the end of the statement for a reason that is not the grammar, so nothing holds the pass's answer to SQLite's there. With the default flags and all the texts, the tests require the numbers of both columns exactly, each on its own (`excused` in `check_test.go`). A change of the oracle or of the pass that excuses more texts, or that moves a text from one column to the other, fails them, and the numbers may be changed only on purpose, with the reason.

The 20,356 supported texts of the corpus are 5 CREATE TABLE, 283 DROP TABLE, 12,681 INSERT, 4,929 SELECT, 595 UPDATE, 541 DELETE, 1,231 CREATE INDEX, and 91 DROP INDEX, each checked in SQLite. Before slice 15 they were 20,350, with 4,923 SELECT: slice 15 added 6 SELECT with a subquery. Before slice 14 they were 20,328, with 12,667 INSERT, 591 UPDATE, and 537 DELETE: slice 14 added 14 INSERT, 4 UPDATE, and 4 DELETE with RETURNING. Before slice 13 they were 17,248, with 1,843 SELECT: slice 13 added 3,080 SELECT with ORDER BY or LIMIT. Before slice 12 they were 17,231: slice 12 added 13 SELECT, 1 UPDATE, and 3 DELETE with a value on the left. Before slice 10 they were 17,118: slice 10 added 92 DROP TABLE and 19 DROP INDEX with IF EXISTS, and 1 CREATE TABLE and 1 CREATE INDEX with IF NOT EXISTS. Before slice 9 they were 15,816: slice 9 added the 1,302 CREATE INDEX and DROP INDEX. Before slice 8 they were 14,692: slice 8 added the 1,124 UPDATE and DELETE. Before slice 7 they were 13,454, with 592 SELECT: slice 7 added 1,238 SELECT with WHERE. Before slice 6 they were 12,736, with 3 CREATE TABLE and 11,950 INSERT: slice 6 added 717 INSERT with NULL as a value and 1 CREATE TABLE with a column that takes NULL. Before slice 5 they were 1,198, with 654 INSERT and 350 SELECT. Most of the 11,296 INSERT that slice 5 added have no list of columns or several values, and the 242 SELECT that it added name columns. The 471 INSERT more than before slice 4 have a real, a string, or a blob as their value. The two that were supported before the empty name was rejected are `DROP TABLE ""` and `INSERT INTO ""("") VALUES(1)`.

The supported mutants of the generator are the same statement with a semicolon or a comment changed, or the same kind of statement with one of these differences:

- Another name of a table or a column, which can be a keyword from the pool of the mutants.
- Another number of columns or of values.
- Another result column, or a name where the statement has `*`.
- The sign of a value dropped.
- A value replaced by a `?`, or a `?` by a value.
- A value of another kind, where the mutant took a literal from the mutants' pool, such as `x'00'` or `1.5`, in place of the value.
- In a select with WHERE: another column of a condition, another operator, a `?` in place of a value or a value in place of a `?`, a value of another kind, or another value.
- In a select with ORDER BY: another column of ORDER BY, another direction of a column, or both, and in 2 mutants another number of columns.
- In an update or a delete: another name of the table, another column of an assignment or of a condition, another operator, a `?` in place of a value or a value in place of a `?`, a value of another kind, or another value.
- In an insert, an update, or a delete with RETURNING: another column of RETURNING, in 106 mutants, or a column where the statement has `*` or `*` where it has columns, in 27.
- In a select, an update, or a delete with a subquery: another subquery, in 134 mutants, where the mutant changed a token inside the parentheses, such as the name of the table or of the column.
- In a create index: another name of the index or of the table, another column, another number of columns, or another order.
- In a drop index: another name of the index.
- In a create table: another default of a column, in 9 mutants, as where the mutant dropped the sign of the literal, and in 1 mutant a DEFAULT NULL where the statement has NOT NULL, where the mutant wrote DEFAULT in place of the NOT.

No supported mutant has a value on the other side of its column. Two have another number of columns of ORDER BY than their statement: the mutant put a comma before the DESC or the ASC of a column, and `desc` or `asc` is then the name of a column of its own. None has an ORDER BY or a LIMIT where its statement has none, or none where its statement has one. None has a RETURNING where its statement has none, none where its statement has one, or another number of columns of RETURNING. None has a subquery where its statement has a literal or a `?`, a literal or a `?` where its statement has a subquery, or another number of subqueries. Six are another kind of statement, which none was before slice 9: 2 are a drop index, where the mutant wrote INDEX in place of the TABLE of a drop table, and 4 are a drop table, where it wrote TABLE in place of the INDEX of a drop index. The generator of the near misses writes no `?`, since the forms of the near misses write the value themselves. `TestGenerate` checks 20,000 statements of the generator, each as the statement it wrote.

Slice 4 moved the pinned numbers of the generated tests twice. First, when the tests of the pass replaced the type or the literal of one statement in two in a wrapper of the generator, the syntax errors of the mutants where SQLite does not say went from 3 to 1, two of the three being mutants of CREATE TABLE statements whose type was then replaced, and the texts not supported where SQLite does not say from 360 to 367, all of them mutants of CREATE TABLE. The excused near misses went from 1,046 to 985: SQLite stops `AUTOINCREMENT` after a column whose type is not INTEGER with the error of a grammar action, `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`, which the oracle excuses, while the near misses of that form whose statement has an INTEGER column parse in SQLite, which refuses AUTOINCREMENT on a WITHOUT ROWID table only later, and are compared.

Then `stmttest` itself learned the types, and the wrapper went: every create table has one of the five types at random, so INTEGER is one in five where it was one in eight, and every insert has a value of a type at random, written by `stmttest.Literal`, whose forms and gaps after a sign differ from the wrapper's. The statements, and so their mutants, are other texts. The syntax error of a mutant where SQLite does not say went from 1 to 0, and the texts not supported where SQLite does not say, all mutants of CREATE TABLE, from 367 to 343. The excused near misses went from 985 to 956, because more of the forms with AUTOINCREMENT have an INTEGER column, which are compared.

Slice 5 moved the pinned numbers of the generated tests again. The generator writes the forms of slice 5 for the tests of the pass, so the statements, and so their mutants and their near misses, are other texts.

- The syntax errors of the mutants where SQLite does not say went from 0 to 85. Of these, 83 are mutants of a create table with a column named twice, where the mutant has a syntax error after that column. SQLite stops at `duplicate column name`, which is the [known gap](#known-gaps) of an action's error before a syntax error. The other 2 have `no such collation sequence`, from a COLLATE that the mutant put on a column.
- The mutants not supported where SQLite does not say went from 343 to 310: 254 with `unknown table option`, 51 with `duplicate column name`, 3 with `no such collation sequence`, and 2 with `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`.
- The excused near misses went from 956 to 3,022. Of these, 2,080 are four new forms on each of the 520 CREATE TABLE statements, for which SQLite gives `table "t" has more than one primary key` while it parses: PRIMARY KEY twice on one column, PRIMARY KEY on a second column with NOT NULL and without it, and a PRIMARY KEY table constraint after the columns. The rest are forms that the near misses had before: 520 with `generated columns cannot be part of the PRIMARY KEY`, 402 with AUTOINCREMENT on a primary key that is not INTEGER, and 20 of two forms with a table constraint.

Slice 6 moved the pinned numbers of the generated tests again. With `Generator.Nulls` on, the generator draws other random numbers, so the statements, and so their mutants and their near misses, are other texts. A throwaway test grouped the excused texts by SQLite's message, on the code before slice 6 and on the code with it, and found no new message.

- The syntax errors of the mutants where SQLite does not say went from 85 to 77: 76 with `duplicate column name` and 1 with `no such collation sequence`, where there were 83 and 2.
- The mutants not supported where SQLite does not say went from 310 to 305: 251 with `unknown table option`, 47 with `duplicate column name`, 6 with `no such collation sequence`, and 1 with `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`.
- The excused near misses went from 3,022 to 2,942, because 502 of the 2,000 statements are CREATE TABLE, where 520 were. Of the 2,942, 2,008 are the four forms with `table "t" has more than one primary key` on each of the 502 statements, 502 have `generated columns cannot be part of the PRIMARY KEY`, 406 have AUTOINCREMENT on a primary key that is not INTEGER, and 26 are of two forms with a table constraint, for which SQLite gives `non-deterministic functions prohibited in index expressions`.
- In the corpus, 718 texts went from not supported to supported, and 8 of its mutants did. The excused numbers of both did not move. The numbers of the sentences of the grammar and of `TestCheck_reals` did not move.

Slice 7 moved the pinned numbers of the generated tests again. With `Generator.Conditions` on, the generator draws other random numbers, so the statements, and so their mutants and their near misses, are other texts. A throwaway test grouped the excused texts by SQLite's message, and found no new kind of message, and no excused text that comes from a select with WHERE.

- The syntax errors of the mutants where SQLite does not say went from 77 to 54, all with `duplicate column name`.
- The mutants not supported where SQLite does not say went from 305 to 284: 239 with `unknown table option`, 43 with `duplicate column name`, 1 with `no such collation sequence`, and 1 with `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`.
- The excused near misses went from 2,942 to 3,003, because 515 of the 2,000 statements are CREATE TABLE, where 502 were. Of the 3,003, 2,060 are the four forms with `table "t" has more than one primary key` on each of the 515 statements, 515 have `generated columns cannot be part of the PRIMARY KEY`, 396 have AUTOINCREMENT on a primary key that is not INTEGER, and 32 have `non-deterministic functions prohibited in index expressions`. Of the 2,000 statements, 477 are SELECT, and 248 of those have a WHERE. The near misses are 125,361, where they were 113,477: SELECT has 32 more forms, and the statements are others.
- In the corpus, 1,238 texts went from not supported to supported, and 54 of its mutants did. The excused numbers of both did not move. The numbers of the sentences of the grammar and of `TestCheck_reals` did not move.

Slice 8 moved the pinned numbers of the generated tests again. With `Generator.Changes` on, one statement in three is an update or a delete, and the generator draws other random numbers, so the statements, and so their mutants and their near misses, are other texts. A throwaway test grouped the excused texts by SQLite's message, and found no new kind of message. Every excused text comes from a CREATE TABLE, and none from an update or a delete.

- The syntax errors of the mutants where SQLite does not say went from 54 to 64, all with `duplicate column name`.
- The mutants not supported where SQLite does not say went from 284 to 196: 160 with `unknown table option`, 33 with `duplicate column name`, and 3 with `no such collation sequence`.
- The excused near misses went from 3,003 to 2,054, because 349 of the 2,000 statements are CREATE TABLE, where 515 were. Of the 2,054, 1,396 are the four forms with `table "t" has more than one primary key` on each of the 349 statements, 349 have `generated columns cannot be part of the PRIMARY KEY`, 291 have `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`, and 18 have `non-deterministic functions prohibited in index expressions`. Of the 2,000 statements, 313 are UPDATE and 349 are DELETE, and 158 and 164 of those have a WHERE. The near misses are 119,881, where they were 125,361: the statements are others, and an update has 64 forms and a delete 41.
- In the corpus, 1,124 texts went from not supported to supported, and 45 of its mutants did. So did 5 of the sentences of the grammar and their mutants. The excused numbers of the three did not move.

Slice 9 moved the pinned numbers of the generated tests again. With `Generator.Indexes` on, one statement in four is a create index or a drop index, and the generator draws other random numbers, so the statements, and so their mutants and their near misses, are other texts. A throwaway test grouped the excused texts by SQLite's message and by the kind of their statement. Every excused text comes from a CREATE TABLE, and none from a create index or a drop index.

- The syntax errors of the mutants where SQLite does not say went from 64 to 39, all with `duplicate column name`.
- The mutants not supported where SQLite does not say went from 196 to 153: 127 with `unknown table option`, 23 with `duplicate column name`, and 3 with `no such collation sequence`.
- The excused near misses went from 2,054 to 1,531, because 261 of the 2,000 statements are CREATE TABLE, where 349 were. Of the 1,531, 1,044 are the four forms with `table "t" has more than one primary key` on each of the 261 statements, 261 have `generated columns cannot be part of the PRIMARY KEY`, 208 have `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`, and 18 have `non-deterministic functions prohibited in index expressions`. Of the 2,000 statements, 257 are CREATE INDEX and 250 are DROP INDEX. The near misses are 99,491, where they were 119,881: the statements are others, a create index has 37 forms, and a drop index has 11.
- In the corpus, 1,302 texts went from not supported to supported, and 62 of its mutants did: 56 CREATE INDEX and 6 DROP INDEX. So did 11 of the sentences of the grammar and their mutants, all DROP INDEX. The excused numbers of the three did not move.

Slice 10 moved the pinned numbers of the generated tests again. With `Generator.Exists` on, the generator draws more random numbers, so the statements, and so their mutants and their near misses, are other texts. A throwaway test grouped the excused texts by SQLite's message and by their form. Every excused text comes from a CREATE TABLE, and no message is new.

- The syntax errors of the mutants where SQLite does not say went from 39 to 29, all with `duplicate column name`.
- The mutants not supported where SQLite does not say went from 153 to 151: 132 with `unknown table option`, 17 with `duplicate column name`, 1 with `no such collation sequence`, and 1 with `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`.
- The excused near misses went from 1,531 to 1,760, because a fifth form gives `table "t" has more than one primary key`: the new `create table if not exists ... w int primary key`, a second PRIMARY KEY after the clause. Of the 2,000 statements, 258 are CREATE TABLE, where 261 were. Of the 1,760, 1,290 are the five forms with that message on each of the 258 statements, 258 have `generated columns cannot be part of the PRIMARY KEY`, 202 have `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`, and 10 have `non-deterministic functions prohibited in index expressions`. The near misses are 108,357, where they were 99,491: the statements are others, and the four kinds of statement that take a clause have 29 forms more.
- In the corpus, 113 texts went from not supported to supported, and 4 of its mutants did, all DROP TABLE. So did 45 of the sentences of the grammar and their mutants: 25 DROP INDEX and 20 DROP TABLE. The excused numbers of the three did not move.

Slice 11 moved the pinned numbers of the generated tests again. With `Generator.Defaults` on, the generator draws more random numbers, so the statements, and so their mutants and their near misses, are other texts. A throwaway tally in `verify` grouped the excused texts by SQLite's message. The excused mutants have the kinds of message that they had before. The excused near misses have one new message, that of a new form.

- The syntax errors of the mutants where SQLite does not say went from 29 to 41: 40 with `duplicate column name` and 1 with `no such collation sequence`.
- The mutants not supported where SQLite does not say went from 151 to 154: 132 with `unknown table option` and 22 with `duplicate column name`.
- The excused near misses went from 1,760 to 2,130, because the new form `default (?)` is excused, and because 272 of the 2,000 statements are CREATE TABLE, where 258 were. SQLite gives `default value of column [...] is not constant` for that form while it parses. Of the 2,130, 1,360 are the five forms with `table "t" has more than one primary key` on each of the 272 statements, 272 have `generated columns cannot be part of the PRIMARY KEY`, 272 have `default value of column [...] is not constant`, 216 have `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`, and 10 have `non-deterministic functions prohibited in index expressions`. The near misses are 117,289, where they were 108,357: the statements are others, and a create table has 36 forms more.
- The numbers of the corpus, of its mutants, and of the sentences of the grammar did not move: none of the texts that the pass supports among them has a DEFAULT.

Slice 12 moved the pinned numbers of the generated tests again. With `Generator.ValueFirst` on, the generator draws more random numbers, so the statements, and so their mutants and their near misses, are other texts. A throwaway tally grouped the excused texts by SQLite's message and by the kind of their statement. Every excused text comes from a CREATE TABLE, and no message is new.

- The syntax errors of the mutants where SQLite does not say went from 41 to 27, all with `duplicate column name`.
- The mutants not supported where SQLite does not say went from 154 to 150: 121 with `unknown table option`, 26 with `duplicate column name`, 2 with `no such collation sequence`, and 1 with `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`.
- The excused near misses went from 2,130 to 1,979, because 253 of the 2,000 statements are CREATE TABLE, where 272 were. Of the 1,979, 1,265 are the five forms with `table "t" has more than one primary key` on each of the 253 statements, 253 have `generated columns cannot be part of the PRIMARY KEY`, 253 have `default value of column [...] is not constant`, 196 have `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`, and 12 have `non-deterministic functions prohibited in index expressions`. Of the 2,000 statements, 243 are SELECT, 232 are UPDATE, and 262 are DELETE, and 125, 118, and 104 of those have a WHERE. No near miss of the three is excused. The near misses are 122,387, where they were 117,289: the statements are others, and a select has 18 forms more, an update 4, and a delete 4.
- In the corpus, 17 texts went from not supported to supported, and 1 of its mutants did. The excused numbers of both did not move. The numbers of the sentences of the grammar and of `TestCheck_reals` did not move.

Slice 13 moved the pinned numbers of the generated tests again, and only by the draws of the generator. With `Generator.OrderBy` on, the generator draws more random numbers, so the statements, and so their mutants and their near misses, are other texts. With `OrderBy` at 0, the three pinned numbers of the list below are those of slice 12 again: 27, 150, and 1,979. A throwaway tally grouped the excused texts by SQLite's message. All come from a CREATE TABLE but one, a select whose WHERE a mutant turned into ON, for which SQLite gives `a JOIN clause is required before ON`, a message that the earlier tallies of the generated tests do not list. For the create tables, [docs/todo/slice-13.md](../../../docs/todo/slice-13.md) records messages of the earlier tallies: `duplicate column name`, `unknown table option`, `generated columns cannot be part of the PRIMARY KEY`, `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`, `table "t" has more than one primary key`, `no such collation sequence`, and `non-deterministic functions prohibited in index expressions`. It does not record how many texts have each.

- The syntax errors of the mutants where SQLite does not say went from 27 to 41.
- The mutants not supported where SQLite does not say went from 150 to 131.
- The excused near misses went from 1,979 to 2,089. The near misses are 137,424, where they were 122,387: the statements are others, and a select has 49 forms more.
- In the corpus, 3,080 texts went from not supported to supported, all SELECT, and 142 of its mutants did. The excused numbers of both did not move. The numbers of the sentences of the grammar and of `TestCheck_reals` did not move.

Slice 14 moved the pinned numbers of the generated tests again, and only by the draws of the generator. With `Generator.Returning` on, the generator draws more random numbers, so the statements, and so their mutants and their near misses, are other texts. With `Returning` at 0, the three pinned numbers of the list below are those of slice 13 again: 41, 131, and 2,089. A throwaway tally grouped the excused texts by SQLite's message and by the kind of their statement. No excused text has a RETURNING. All come from a CREATE TABLE but two, mutants of a select whose WHERE became ON, for which SQLite gives `a JOIN clause is required before ON`, the message that slice 13 met first.

- The syntax errors of the mutants where SQLite does not say went from 41 to 45, all with `duplicate column name`.
- The mutants not supported where SQLite does not say went from 131 to 167: 126 with `unknown table option`, 37 with `duplicate column name`, 2 with `a JOIN clause is required before ON`, 1 with `no such collation sequence`, and 1 with `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`.
- The excused near misses went from 2,089 to 1,983. Of the 2,000 statements, 253 are CREATE TABLE. Of the 1,983, 1,265 are the five forms with `table "t" has more than one primary key` on each of the 253 statements, 253 have `generated columns cannot be part of the PRIMARY KEY`, 253 have `default value of column [...] is not constant`, 198 have `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`, and 14 have `non-deterministic functions prohibited in index expressions`. Of the 2,000 statements, 249 are INSERT, 265 are UPDATE, and 264 are DELETE, and 82, 85, and 82 of those have a RETURNING. No near miss of the three is excused. The near misses are 141,834, where they were 137,424: the statements are others, and an insert has 15 forms more, an update 5, and a delete 4.
- In the corpus, 22 texts went from not supported to supported, 14 INSERT, 4 UPDATE, and 4 DELETE, and 1 of its mutants did. The excused numbers of both did not move. The numbers of the sentences of the grammar and of `TestCheck_reals` did not move.

Slice 15 moved the pinned numbers of the generated tests again, and only by the draws of the generator and by one new form of the near misses. With `Generator.Subquery` on, the generator draws more random numbers, so the statements, and so their mutants and their near misses, are other texts. A throwaway tally grouped the excused texts by SQLite's message. Every message is one that the earlier tallies have for a CREATE TABLE. No excused text has a subquery in a WHERE: the only excused texts with a subquery are the near misses of the new form `default ((select 1))`.

- The syntax errors of the mutants where SQLite does not say went from 45 to 35: 34 with `duplicate column name` and 1 with `no such collation sequence`.
- The mutants not supported where SQLite does not say went from 167 to 139: 111 with `unknown table option`, 26 with `duplicate column name`, 1 with `no such collation sequence`, and 1 with `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`.
- The excused near misses went from 1,983 to 2,275, because the new form `default ((select 1))` is excused, and because 258 of the 2,000 statements are CREATE TABLE, where 253 were. SQLite gives `default value of column [...] is not constant` for that form while it parses, as it does for `default (?)`. With `Subquery` at 0, the forms that the near misses had before give 1,983 again, and the new form 253 more. Of the 2,275, 1,290 are the five forms with `table "t" has more than one primary key` on each of the 258 statements, 516 are the two forms with `default value of column [...] is not constant`, 258 have `generated columns cannot be part of the PRIMARY KEY`, 195 have `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`, and 16 have `non-deterministic functions prohibited in index expressions`. Of the 2,000 statements, 222 are SELECT, 284 are UPDATE, and 266 are DELETE, and 45, 52, and 50 of those have a subquery. The near misses are 153,278, where they were 141,834: the statements are others, and a select has 43 forms more, an update 5, a delete 5, an insert 2, and a create table 1.
- In the corpus, 6 texts went from not supported to supported, all SELECT with a subquery. Its excused numbers did not move. The numbers of the mutants of the corpus, of the sentences of the grammar, and of `TestCheck_reals` did not move.

`TestCheck_reals` checks 100,000 texts of real literals: SQLite reads 81,351 as reals and 18,649 as infinities, and the pass answers every one as SQLite gives it. For 6,998 of the texts Go's `strconv.ParseFloat` gives another float than SQLite.

Fuzzing, before slice 4: `FuzzCheck` made 202,673,795 executions in 40 minutes on 6 workers, and on the code after the review 87,983,456 in 30 minutes on 4 workers. `FuzzGenerate` made 11,418,100 executions in 15 minutes on 4 workers; each checks a statement of the generator and its mutant in SQLite. None of the runs found anything. On the code of slice 4, short local runs found nothing: `FuzzLiteral` made 7,192,877 executions in 1 minute, `FuzzCheck` 4,915,832 in 45 seconds, and `FuzzGenerate` ran 30 seconds. On the code of slice 7, `FuzzLiteral` ran 30 seconds and found nothing. CI runs them longer.

Mutation testing, before slice 4, on 2026-11-18 after `StartedTable` moved to `stmttest`, which is not mutated: of 896 mutants, 117 do not compile, 766 are killed, 12 time out, and 1 is equivalent; none is unexplained. Before, with `StartedTable` in the package, of 1,122 mutants, 123 did not compile, 970 were killed, 14 timed out, and 15 were equivalent, 14 of them in `StartedTable`. See [`internal/sql/mutation`](../mutation/README.md#the-pass-after-the-parser). Mutation testing of the code of slice 4 is for a later task. So are mutation testing and long fuzz runs of the code of slice 5. Mutation testing and long fuzz runs of the code of slice 6 are for a later task too, and so are those of the code of slice 7, of slice 8, of slice 9, of slice 10, of slice 11, of slice 12, of slice 13, and of slice 14.

Statement coverage is 99.4% under `-short`, measured with the code of slice 12. With the code of slice 11 it was 99.5%, and the last place of the list below is new. The code of the update and the delete is covered completely, and so are that of the create index and the drop index, that of the two clauses of slice 10, and that of a value on the left in the recognizer. The generated tests do not reach the panic of `CreateTable.Key()` for a table without a primary key, which the pass never gives; the unit test `TestCreateTable_Key` covers it. The places that are not covered:

- The defaults of `literalName` and `unaryName` for a kind of token that the grammar never puts there.
- The name of a node for one of its own tokens whose field has no name, which no offset reaches.
- The empty name of the zero `Type`.
- The `isValue` marker methods of the five value types, in `stmt.go`.
- The answer of `Operator.Swapped` for a test for NULL and for no operator, which no test asks for.

### Run times

Measured on an Apple M4 with 10 cores while other tests ran; the number under the address sanitizer is from a Linux container in Docker on the same machine.

- `go test ./internal/sql/stmt` takes about 50 s with the code of slice 15, in one run with `-v`. With the code of slice 14 it took about 40 s, in one run with `-v`. With the code of slice 13 it took about 100 s, in one run with `-v`. With the code of slice 12 it took about 90 s, in one run. With the code of slice 11 it took about 100 s, in one run while other tests ran. With the code of slice 10 it took about 70 s, in one run. With the code of slice 9 it took about 60 s, in one run. With the code of slice 8 it took about 80 s, in one run while little else ran. With the code of slice 7 it took about 90 s. With the code of slice 5 it took about 70 s. Before slice 5 it took about 25 s, `-short` about 3 s, and `-race` about 11 s.
- Under the address sanitizer it took about 43 s before slice 4, and `make test-asan` passed. It is not measured again since.
- `BenchmarkCheck` checks the corpus in about 0.3 s, at about 32 MB/s. `BenchmarkCheck_long` shows a time linear in the length from 10,000 to 100,000 result columns and rows, at about 13 MB/s, and faster for 1,000; a long comment goes at several hundred MB/s.
