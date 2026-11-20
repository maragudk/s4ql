# The pass after the parser

`internal/sql/stmt` takes SQL text and gives one of four answers:

1. **SQLite's syntax error**, as a `*sqlerr.Error` of [`internal/sql/sqlerr`](../sqlerr/sqlerr.go), the one type of SQLite's errors that s4ql gives, which the engine gives too: the message of SQLite's parser, with the result code 1 and the extended result code 1, when the text is a syntax error in SQLite. This holds also when the text is SQL that s4ql does not support. Only the checks before parsing come first: a text longer than SQLite's limit, with a NUL byte, or with bytes that are not UTF-8 is not supported, whatever else it holds. The byte offset is kept as extra information; it is not part of the promise.
2. **"Not supported"**, as a `*stmt.NotSupportedError`, when SQLite's parser accepts the text and the text is not, token for token, a statement of [slice 1](#slice-1), with the parameter of slice 2, the column types and the literals of slice 4, the several columns of slice 5, and the NULL of slice 6. The error matches `stmt.ErrNotSupported` with `errors.Is`, its message starts with `s4ql: not supported: `, and it carries no result code of SQLite.
3. **No statement and no error**, for a text that is empty or holds only spaces, comments, and semicolons. SQLite prepares such a text into no statement and no error.
4. **A supported statement**: a `*stmt.CreateTable`, `*stmt.Insert`, `*stmt.Select`, or `*stmt.DropTable`.

The function is `stmt.Check(src) (stmt.Statement, error)`. The rule of the project is in [docs/roadmap.md](../../../docs/roadmap.md): nothing is supported until it is listed. This package is the place where that rule is enforced, and the [definition](#slice-1) below is the precise form of the roadmap's slices 1 and 2, of the column types and the literals of slice 4, of the several columns of slice 5, and of the NULL of slice 6, as the pass implements them.

## The typed form

A supported statement is one of four types. The engine is built on them and never reads the syntax tree.

| Type | Fields | Meaning |
| --- | --- | --- |
| `CreateTable` | `Table`, `Columns []stmt.Column` | Create a table with one or more columns, in the order of the text. Exactly one column is its primary key. The table is STRICT and WITHOUT ROWID. |
| `Insert` | `Table`, `Columns []stmt.Name`, `Values []stmt.Term` | Insert one row. `Columns` is the list of columns of the statement, in the order of the text, and nil when the statement has no list: the values are then for the columns of the table, in the order of the table. `Values` holds the values of the row, in the order of the text. |
| `Select` | `Table`, `Columns []stmt.Name` | Read every row of one table. `Columns` holds the names of the select list, in the order of the text, each as often as the list names it. It is nil for `*`, which is every column in the order of the table. |
| `DropTable` | `Table` | Drop one table. |

Every type has `Span`, the bytes of the statement from its first token to the end of its semicolon, or of its last token if it has none.

A `stmt.Column` is a column as CREATE TABLE declares it. It has four fields:

- `Name` is the name of the column.
- `Type` is the type of the column.
- `PrimaryKey` reports whether the column is the primary key of its table.
- `NotNull` reports whether the column says NOT NULL. A column that is not the primary key and does not say it takes NULL. The primary key can say it too, and SQLite records the same table with it and without it: the primary key of a WITHOUT ROWID table never takes NULL.

`CreateTable.Key()` gives the index in `Columns` of the primary key. `CreateTable.ColumnsError()` gives the error that SQLite gives for the columns of the statement, or nil; [CREATE TABLE](#create-table) says when it is not nil.

A `stmt.Term` is one value of the row of an insert. It has three fields:

- `Value` is the value of a literal. It is nil for a parameter. For the literal NULL it is `stmt.Null{}`, never nil.
- `Parameter` is the number of the parameter that gives the value, from 1, or 0 if the value is a literal.
- `Span` is the bytes of the value, with its sign, or of the parameter.

So the `Term` of a NULL has `Value: stmt.Null{}` and `Parameter: 0`, and the `Term` of a parameter has a nil `Value` and a `Parameter` from 1. The two cannot be taken for each other.

A `stmt.Type` is the type of the column as the statement declares it: `TypeInt`, `TypeInteger`, `TypeReal`, `TypeText`, or `TypeBlob`. Its `String` is the name that SQLite records: `INT`, `INTEGER`, `REAL`, `TEXT`, or `BLOB`. INT and INTEGER stay two types, as SQLite records them, though they behave the same in a STRICT, WITHOUT ROWID table. The zero value is no type.

A `stmt.Value` is the value of a literal, and one of five types: `stmt.Integer`, an `int64`; `stmt.Real`, a `float64`; `stmt.Text`, a `string`; `stmt.Blob`, a `[]byte`, which is never nil, also when it is empty; and `stmt.Null`, an empty struct, which is the value of the literal NULL. [INSERT](#insert) says which value each literal has.

A `Name` has three fields, because SQLite prints names in different forms in its messages:

- `Raw` is the name as written, with its quotes. SQLite prints it in `table "t1" already exists`: `create table "t1" …` twice gives `table "t1" already exists`, and `create table T1 …` after `t1` gives `table T1 already exists`.
- `Value` is the name without its quotes, with `""` read as `"`. SQLite prints it in `no such table: t2`, in `table t1 has no column named v2`, and in `no such column: v2`: `insert into "t""1" ("V""2") values (1)` gives `table t"1 has no column named V"2`. The names that CREATE TABLE stores are its `Value`s, and SQLite prints them in `UNIQUE constraint failed: t1.v1`: after `create table Foo (Bar int primary key) …`, `insert into FOO (BAR) values (1)` twice gives `UNIQUE constraint failed: Foo.Bar`.
- `Offset` is the byte offset of the name's token. SQLite gives the offset of the name with `already exists`.

`Name.Fold` gives the value with the ASCII letters in lower case. SQLite takes two names for the same name when their folds are equal. It folds no other letter: `ÆØÅ` and `æøå` are two tables. This holds for the names of columns too: the pass compares the columns of a CREATE TABLE and of the list of an INSERT by their folds.

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

- An optional keyword, such as TEMP, or an optional clause, such as WHERE, is a field of the tree that the recognizer does not read. Its token is at the cursor when the recognizer takes the next token it expects, and the take fails.
- A node of a type that the recognizer does not know is not entered, so none of its tokens is taken.
- After the statement, every token of the tree must be taken. This check is generic: it holds for any kind of command and any node, also one that the tree gains later.

By the parser's ownership rule, which its tests check on every statement, these are exactly the tokens of `ast.AllTokens` of the tree. The gate does not depend on that rule: the tokens come from the text, so a token of the text that no node holds is never taken, and a token that a node holds and the text does not have cannot be taken.

## Slice 1

A statement is supported if it is one of the four forms below, token for token. Slice 2 of the roadmap adds one form of the value of INSERT, a parameter, and this section defines it under [INSERT](#insert). Slice 4 adds the column types other than INT and the literals other than an integer, and this section defines them under [CREATE TABLE](#create-table) and [INSERT](#insert). The rest of slice 4, the arguments and the check of a value against the type of its column, is for the engine and the driver. Slice 5 adds a table of several columns, an insert of several values with or without a list of columns, and a select of named columns, and this section defines them under [CREATE TABLE](#create-table), [INSERT](#insert), and [SELECT](#select). What such a statement means for a table depends on the database, so it is for the engine: whether the columns exist, and whether the values are as many as the columns. Slice 6 adds a column that takes NULL and the literal NULL as a value of an insert, and this section defines them under [CREATE TABLE](#create-table) and [INSERT](#insert). Whether a column takes the NULL that an insert gives it depends on the table, so it is for the engine too. The rest of slice 6, the nil argument and the NULL that is stored and read, is for the engine and the driver, which run the forms of slice 6 since 2026-11-20. The tables give the tokens in order. A gap between two tokens may hold any spaces and comments. A keyword may be in any case of its letters.

The roadmap's restrictions and these slices define the forms. Where the roadmap leaves the form open, the rule is: when in doubt, it is not supported. A form is supported only if a test against SQLite shows that SQLite treats it exactly as the plain form, or the roadmap names it.

### CREATE TABLE

```
CREATE TABLE table ( column-def [, column-def]... ) STRICT , WITHOUT ROWID
CREATE TABLE table ( column-def [, column-def]... ) WITHOUT ROWID , STRICT
```

A `column-def` is one of these:

```
column type
column type PRIMARY KEY
column type NOT NULL
column type PRIMARY KEY NOT NULL
column type NOT NULL PRIMARY KEY
```

- `type` is one bare word, `int`, `integer`, `real`, `text`, or `blob`, in any case of its letters.
- `int` and `integer` are two types. In a STRICT, WITHOUT ROWID table, `integer primary key` is not an alias of the rowid, since there is none, and it behaves as `int primary key` does. But SQLite records the type as `INTEGER`, not `INT`, so it is not the same table, and `Column.Type` keeps the two apart.
- Exactly one column says PRIMARY KEY. It can be at any position.
- Every other column says NOT NULL or no constraint. A column without a constraint takes NULL, which is slice 6: `pragma_table_xinfo` gives `notnull` 0 for it, and `Column.NotNull` is false.
- The primary key can say NOT NULL too, before or after PRIMARY KEY. SQLite records the same table with it and without it: `pragma_table_xinfo` gives `notnull` 1 for the primary key of a WITHOUT ROWID table either way, and the primary key never takes NULL. `Column.NotNull` keeps what the statement says.
- Both orders of the options are supported. SQLite records the same table for both: `pragma_table_list` gives `wr` 1 and `strict` 1, and `pragma_table_xinfo` the same columns, for `strict, without rowid` and for `without rowid, strict`. The tests compare the schema that each statement makes with that of the plain form, as SQLite's PRAGMAs record it: the tables and their options, columns, and indexes. The text that `sqlite_schema` keeps is not compared, since SQLite keeps it as written.
- The roadmap's restrictions say that all tables are STRICT and WITHOUT ROWID, and [research/sql-front-end.md](../../../docs/research/sql-front-end.md) §3.3 lists a table without the options as rejected. The statement must therefore say both: without them, SQLite makes a table that is not STRICT or has a rowid, and the roadmap's principle is to reject, never to reinterpret.

Two faults of the columns are supported by the pass, and SQLite's error for them is left to the engine:

- A column that has the name of an earlier column, as `Name.Fold` compares names.
- More than 2,000 columns, which is SQLite's limit, `SQLITE_MAX_COLUMN`.

SQLite raises both errors in `sqlite3AddColumn`, while it parses. But `sqlite3StartTable` has looked for the table before that. On a database that has a table of the name, SQLite gives `table t already exists`, and never meets the fault of the columns. So SQLite's answer depends on the database, and the spec accepts a difference from SQLite only for a syntax error. The pass therefore supports the statement, and `CreateTable.ColumnsError()` gives the error that SQLite gives on a database without the table, with the result code 1 and the extended result code 1:

- `duplicate column name: v1`, for the first column whose name an earlier column has. The name in the message is the value of the name of the later column.
- `too many columns on t1`, for the 2,001st column, also when it has the name of an earlier column. The name in the message is the value of the name of the table.

SQLite adds the columns in order, so the first fault in the order of the text decides the error. The engine gives the error after its check of the table. A statement that is not supported for another reason stays not supported, also when SQLite would first give the error of a column named twice: `create table t (a int primary key, a int primary key) strict, without rowid` is `second PRIMARY KEY`, where SQLite gives `duplicate column name: a`, as an unknown type is not supported where SQLite gives `unknown datatype`. The tests check `ColumnsError()` against SQLite on a database with the table and on a database without it.

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
- NOT NULL twice, NOT NULL with ON CONFLICT, DEFAULT, also `default null`, and every other column constraint, each with the name of its node, such as `NULL or NOT NULL constraint` and `DEFAULT`. Also `CONSTRAINT name`, ASC or DESC, ON CONFLICT, and AUTOINCREMENT.
- A table constraint, such as `primary key (a, b)` or `unique (b)`, as `table constraint`, at the comma after the last column.
- `STRICT` or `WITHOUT ROWID` alone, none, either twice, a third option, an empty first option (`) , strict, without rowid`), and `"strict"` or `without "rowid"`, which SQLite rejects with `unknown table option`.
- TEMP, IF NOT EXISTS, a schema name, and `AS SELECT`.

### INSERT

```
INSERT INTO table VALUES ( value [, value]... )
INSERT INTO table ( column [, column]... ) VALUES ( value [, value]... )
```

Without a list of columns, the values are for the columns of the table, in the order of the table. With a list, the columns can be in any order.

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
- `NOT NULL constraint failed: t1.v2`, with the extended result code 1299, for a column that says NOT NULL, or the primary key, when the list leaves it out or its value is NULL. The message names the first such column in the order of the table.

A column that takes NULL gets NULL when the list leaves it out.

A value can also be a parameter, a plain `?`, which is slice 2. SQLite gives a `?` the number after the largest one before it, and every parameter of a supported statement is a plain `?`. So the k-th `?` of the statement has the number k. For a parameter, `Term.Parameter` is that number, `Term.Value` is nil, and `Term.Span` is the span of the `?`. `Parameters()` of every statement gives the number of its parameters, as `sqlite3_bind_parameter_count` gives it: the number of `?` in an insert, and 0 for every other statement. Which argument binds to a parameter, and what an argument may be, is for the engine and the driver to check; the pass sees only the text.

A column that the list names twice, as `Name.Fold` compares names, is not supported, as `column named twice`. SQLite accepts such a statement and uses the first value for the column: `insert into t (a, b, a) values (3, 'z', 4)` stores 3 in `a`.

Not supported: REPLACE, INSERT OR, WITH, an alias, DEFAULT VALUES, a second row, SELECT, a compound, an upsert, and RETURNING.

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

A parameter anywhere else is not supported for what holds it: a WHERE clause, a LIMIT clause, a result column, a DEFAULT, and so on.

SQLite's own errors of a parameter are not reproduced, because every such parameter is not supported: `?0` and `?32767` give `variable number must be between ?1 and ?32766` in SQLite, and the pass gives `numbered parameter`. A `#` and a digit, as in `#1`, and two parameters without a comma between them, as in `(? ?)`, are syntax errors, which the pass gives as SQLite gives them.

### SELECT

```
SELECT * FROM table
SELECT column [, column]... FROM table
```

- `*` is every column, in the order of the table.
- A `column` is the name of a column alone, without a table before it, as SQLite reads it in the select list. The list can name the columns in any order, and it can name a column more than once.
- A keyword that SQLite reads as the name of a column in the select list is a name there, such as `abort`.

The pass does not know the table, so two facts of SQLite are for the engine:

- SQLite names a result column by the name as the table stores it, not as the statement writes it. With a column `b`, `select B from t` gives a column named `b`.
- For a name that the table does not have, SQLite gives `no such column: v`, with the value of the name. For such a name in double quotes, it gives `no such column: "v" - should this be a string literal in single-quotes?`, because the pinned build of SQLite has `DQS=0`.

Not supported as a result column:

- `*` with another result column, as `* and another result column`.
- Any other result column, such as an expression, a literal, also NULL, `t.v`, or `t.*`, as `result column other than a column or *`.
- An alias, as `alias`.
- The bare word TRUE or FALSE, as `TRUE or FALSE`. SQLite reads it as 1 or 0 where the table has no column of that name. `"true"` in double quotes is a name.

Not supported: DISTINCT or ALL, no FROM, a second table or a join, an alias of the table, INDEXED BY or NOT INDEXED, a table in parentheses, a subquery, a table-valued function, ON or USING, WHERE, GROUP BY, HAVING, WINDOW, ORDER BY, LIMIT, a compound, WITH, VALUES.

### DROP TABLE

```
DROP TABLE table
```

Not supported: IF EXISTS, DROP VIEW, DROP INDEX, DROP TRIGGER, and a schema name.

### Names

`table` and `column` are names:

- A bare name, such as `t1` or `été`, or a keyword that SQLite reads as a name in that place, such as `abort`. The parser has decided which keywords are names, as SQLite's grammar does. The value of a bare name is its text.
- A name in double quotes, with `""` for a quote inside. Its value is the text between the quotes, with `""` read as `"`.

Not supported: the empty name `""`, in every place, though SQLite takes it for a name and creates a table and a column of that name. The storage core and the format of the commit object do not hold a table or a column without a name (`checkIdentifier` in `internal/format/check.go`), and the pass is the one gate that decides what is supported. It is the only name that SQLite and this pass accept and the core refuses: the core's other rules on names are valid UTF-8, no NUL, and no reserved prefix, which the checks before parsing and the reserved names cover already. `TestEngine_namesOfTheCore` in `internal/engine` checks, against the real core, that every name of the pools of the generator and thousands of random names in double quotes, of every kind of character, are taken by the core wherever the pass supports them, as SQLite takes them.

Not supported: a name in `[brackets]`, in backticks, or in single quotes, which SQLite accepts in some places. The roadmap's restrictions reject all three.

A table name is reserved, and makes any statement that names it not supported, if it is one of these, in any case of the ASCII letters:

- A name that starts with `sqlite_` or `s4ql_`. The roadmap's restrictions reject these, for every kind of statement, so `object name reserved for internal use` and `table sqlite_master may not be dropped` are not reproduced, and `sqlite_schema` cannot be read.
- The name of an **eponymous virtual table** of SQLite: `json_each`, `json_tree`, `jsonb_each`, `jsonb_tree`, and `pragma_` followed by the name of each PRAGMA that gives rows, such as `pragma_table_list`. SQLite gives every connection these tables without a CREATE. With a table of that name missing, `select * from json_each` gives rows in SQLite, `drop table json_each` gives `table json_each may not be dropped`, and `insert into json_each (key) values (1)` gives `table json_each may not be modified`; an engine that knows only its own tables would give `no such table`. So the pass rejects these names rather than let the engine give an answer that is not SQLite's. `pragma_x`, which names no PRAGMA, is a name like any other. The list holds 59 PRAGMAs. `TestReservedName` checks the list against SQLite: of every PRAGMA of the build, every module, and every module that SQLite registers when a statement names it, exactly those in the list can be read without a CREATE, in any case of the letters. The modules that SQLite registers when a statement names them are read from `sqlite3LocateTable` in the amalgamation by hand: a later version of SQLite that adds one there needs the test, and the list, extended.

A column name is never reserved: `sqlite_v` is a column name like any other.

### The statement and the text

- The statement may end with a semicolon. Before it, any number of empty statements, spaces, and comments may come, which SQLite skips. After it, only spaces, comments, and semicolons may come.
- EXPLAIN and EXPLAIN QUERY PLAN are not supported.
- A text with a second statement is not supported, at the second statement's first token, even if the first statement is supported and the second is a syntax error.

### The errors that SQLite raises in grammar actions

The parser accepts every statement whose only fault is an error that one of SQLite's grammar actions raises, such as `unknown table option`. Its README lists them.

Two can occur for a supported statement, a CREATE TABLE of several columns:

- `duplicate column name: %s` needs a column with the name of an earlier column.
- `too many columns on %s` needs more than 2,000 columns.

SQLite gives `table %s already exists` in place of either when a table of the name exists, so the pass supports the statement and gives the error through `CreateTable.ColumnsError()`; see [CREATE TABLE](#create-table).

`too many SQL variables` needs more than 32,766 parameters, `SQLITE_MAX_VARIABLE_NUMBER`, which an insert of slice 5 can have. SQLite gives it while it parses, at the 32,767th `?`, whatever the database holds. By the rule for such errors, the pass rejects the statement as not supported, as `parameter beyond SQLite's limit`, at that `?`. `TestCheck` checks that an insert of 32,766 parameters is supported and parses in SQLite, and that one of 32,767 is not supported where SQLite stops early.

None of the others can occur for a supported statement:

- `unknown table option` needs an option other than `strict` or `without rowid`, as bare words.
- `object name reserved for internal use` needs a name that starts with `sqlite_`, which is reserved.
- `unknown database` and `temporary table name must be unqualified` need a schema name or TEMP.
- `table "t" has more than one primary key` needs a second PRIMARY KEY, which is not supported.
- `unknown datatype for %s.%s: "%s"` needs a type that is not one of SQLite's standard ones in a STRICT table, which is not supported.
- A misplaced digit separator and a variable `#N` are syntax errors, which the parser gives.
- `variable number must be between ?1 and ?%d` needs a numbered parameter, such as `?0`, which is not supported.
- The rest need clauses, expressions, or commands that no supported form has.

So every statement with one of these other errors is not supported, and SQLite's message for it is not reproduced. A text in which an error of a grammar action comes before a syntax error is a case of its own: SQLite stops at the action's error, and the oracle does not say whether the text parses, while the parser goes on and gives the later syntax error. The pass then gives that syntax error, which is not SQLite's message; see [Known gaps](#known-gaps).

## Not supported

The message of the error is `s4ql: not supported: ` with the name of what is not supported and its offset, as in `s4ql: not supported: WHERE clause (at offset 16)`. The offset is in bytes from the start of the text.

The rule for the offset: it is that of the first token, in the order of the text, that is not part of a supported statement. When a part that the statement needs is missing, such as PRIMARY KEY, the offset is that of the token where the part would go, or the end of the statement if no token is left.

The rule for the name, in this order:

1. The checks before parsing, the limits of the parser, and a second statement have names of their own.
2. Where the recognizer expects a token and finds one of another form, or finds that a part is missing, the name is that of the check, in the first table below.
3. Otherwise the token at the offset belongs to a construct that the recognizer did not accept. From the root of the tree, the walk goes down through the nodes that the recognizer accepted. If the token is one of such a node's own tokens, the name is that of its field, in the second table. If it is in a child that the recognizer did not accept, the name is that of the child's type, in the third table. So the name is that of the outermost construct around the token that is not supported: `values (1 + 2)` gives `binary operator`, not `literal`.

A test checks that every name the pass gives in any test is in these tables.

### Names of the checks

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
| `table constraint` | CREATE TABLE has a table constraint. |
| `column without a type` | The column has no type. |
| `column type other than INT, INTEGER, REAL, TEXT, or BLOB` | The column's type is not one of the bare words `int`, `integer`, `real`, `text`, and `blob`. |
| `NULL constraint` | A column says NULL, without NOT before it. SQLite reads it as no constraint. |
| `second PRIMARY KEY` | A column says PRIMARY KEY after an earlier one did, or says it twice. SQLite gives `table "t" has more than one primary key`. |
| `table without PRIMARY KEY` | No column says PRIMARY KEY. SQLite gives `PRIMARY KEY missing on table t` after it has parsed the statement. |
| `table option` | An option is not STRICT or WITHOUT ROWID, or comes again, or is a third. |
| `table without STRICT and WITHOUT ROWID` | CREATE TABLE has no options. |
| `table without STRICT` | CREATE TABLE has WITHOUT ROWID only. |
| `table without WITHOUT ROWID` | CREATE TABLE has STRICT only. |
| `REPLACE` | The statement is REPLACE INTO. |
| `column named twice` | The column list of INSERT names a column that it named before, as SQLite compares names. SQLite takes the first value for it. |
| `INSERT with SELECT` | INSERT has a SELECT in place of VALUES. |
| `+ sign` | The value has a `+` sign, but before NULL. |
| `~ operator` | The value has a `~`, also after its `-`. |
| `NOT operator` | The value has a NOT, also after its `-`. |
| `second sign` | The value has a `+` or `-` after its `-`. |
| `hex literal` | The value is a hex literal. |
| `number with a digit separator` | The value has a digit separator. |
| `integer outside 64 bits` | The value does not fit a signed 64-bit integer. |
| `infinite real number` | The value is a real number that SQLite reads as an infinity, such as `1e999`. |
| `string after a sign` | The value is a string after its `-`. |
| `blob after a sign` | The value is a blob after its `-`. |
| `TRUE or FALSE` | The value of INSERT, or a result column of SELECT, is the bare word TRUE or FALSE, which SQLite reads as 1 or 0. |
| `NULL after a sign` | The value is NULL after a `-` or a `+`, which SQLite reads as NULL. |
| `current date or time` | The value is CURRENT_DATE, CURRENT_TIME, or CURRENT_TIMESTAMP. |
| `numbered parameter` | The value is a parameter with a number, such as `?1`. |
| `named parameter` | The value is a parameter with a name: `:name`, `@name`, `$name`, or `#name`. |
| `parameter beyond SQLite's limit` | The value is the 32,767th parameter of the statement. SQLite gives `too many SQL variables`. |
| `parameter after a sign` | The value is a parameter after its `-`. |
| `result column other than a column or *` | SELECT has a result column that is not the bare name of a column or `*`, such as `1`, `v + 1`, `t.v`, or `t.*`. |
| `* and another result column` | SELECT has `*` and another result column. |
| `SELECT without FROM` | SELECT has no FROM. |
| `DROP VIEW` | The statement is DROP VIEW. |
| `DROP INDEX` | The statement is DROP INDEX. |
| `DROP TRIGGER` | The statement is DROP TRIGGER. |

### Names of fields

These are the tokens of a node that the recognizer accepted, in a field that no supported form has. Only the first token of such a construct can be at the offset, so only the fields that hold a first token are named.

| Name | Fields |
| --- | --- |
| `EXPLAIN` | `Statement.Explain`, for EXPLAIN and EXPLAIN QUERY PLAN |
| `schema name` | `QualifiedName.Schema`, in any node |
| `TEMP` | `CreateTable.Temp` |
| `IF NOT EXISTS` | `CreateTable.IfNotExists` |
| `empty table option` | `CreateTable.OptionCommas`, for a comma before the first option |
| `CREATE TABLE AS` | `CreateTable.As` |
| `ASC or DESC` | `PrimaryKeyConstraint.Order` |
| `AUTOINCREMENT` | `PrimaryKeyConstraint.Autoincrement` |
| `IF EXISTS` | `Drop.IfExists` |
| `INSERT OR` | `Insert.Or` |
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
| `INDEXED BY or NOT INDEXED` | `IndexedBy` |
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
6. Extend the generator to every new form, and `checkMeaning` in `stmt_test.go` to compare each new statement with what SQLite does. The generator in [`stmttest`](stmttest/stmttest.go) is shared with the tests of the engine and the driver. So put a new form behind an option of the generator, as `Generator.Columns` is for slice 5 and `Generator.Nulls` for slice 6, until the engine supports it. With the option off, the generator must write what it wrote before and draw the same random numbers, so that the generated tests of the engine and the driver draw the same calls.
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
  - CREATE TABLE: the schema that SQLite records for the text is that of the canonical statement built from the typed form. `pragma_table_list` gives the name of the table, the number of its columns, `wr` 1, and `strict` 1. `pragma_table_xinfo` gives, for every column, its name, the name of its `Type`, `notnull` 1 for the primary key and for a column that says NOT NULL and 0 for a column that takes NULL, and `pk` 1 for the primary key and 0 for every other column. The second run of the text gives `table <Raw> already exists`.
  - CREATE TABLE with a `ColumnsError()`: on an empty database SQLite gives that error, with its message and the code 1, for the text and for the canonical statement. On a database that has the table, with the case of its name swapped, SQLite gives `table <Raw> already exists`.
  - INSERT, the values: the value of each literal is exactly what SQLite gives for `select` and the text of the literal, which the test reads from `Term.Span`. It has the same type and the same value: an integer by its number, a real by its 64 bits, and text and a blob byte by byte. The literal NULL is compared with what SQLite gives for `select null`, as every literal is.
  - INSERT, the table of the check: its name has the case of its ASCII letters swapped. Its columns are those of the list, with the case swapped, in the reverse order of the list, so that a value goes to its column by the name. Without a list, they are `c1` to `cn`, in the order of the values. The column of the first value that is not NULL is the primary key. Each column has the type of its value: INT for an integer and for a parameter, REAL, TEXT, or BLOB. The column of a NULL has one of the five types and no NOT NULL. Every other column that is not the primary key says NOT NULL. The parameters are bound to integers at or near the limits of 64 bits that differ from each other, so that a value in the wrong column is seen.
  - INSERT, what SQLite does: on that table, the text stores each value in its column, as the canonical statement does, and the test reads every column. A second run gives `UNIQUE constraint failed` with the names as the table stores them. Without the table the text gives `no such table: <Value>`. When the list and the values differ in number, it gives `N values for M columns`. Without a list, on a table with one more column, it gives `table <Value> has N+1 columns but N values were supplied`. With a list, on a table without the first column of the list, it gives `table <Value> has no column named <Value>`. The stored value is the value, but for a real zero: SQLite stores a real with an integer value as an integer in a REAL column, so `-0.0` reads back as `0.0`.
  - INSERT with a NULL, in `checkNotNull`: the table has the same columns in the same order, and every column says NOT NULL or is the primary key. If no value can be the key, the first column of the table is the key. SQLite gives `NOT NULL constraint failed: <table>.<column>`, with the extended code 1299, for the first column in the order of the table that gets a NULL, with the names as the table stores them. An insert of only NULLs has no column that can be the key, so no table stores its row. The check of the stored values is skipped for it, and every check of an error runs.
  - SELECT: the table of the check has the columns that the text names, once each, with the case swapped, and one more column. For `*` it has three columns, with the primary key second. It holds three rows, beside another table. The names of the result columns and the rows must be SQLite's: the names as the table stores them, and the rows in key order. The canonical statement gives the same. Without the table the text gives `no such table: <Value>`. On a table without the first name of the list it gives `no such column: <Value>`, or the message with the question about a string literal for a name in double quotes.
  - DROP TABLE: the text drops the named table and leaves the other, as the canonical statement does; without the table it gives `no such table: <Value>`.
  - The canonical statement is supported and gives the same statement, the raw text of each name is at its offset, and the span ends where SQLite's prepare ends the statement. The canonical statement writes every name in double quotes, NOT NULL after PRIMARY KEY, an integer in decimal, a real in Go's shortest form with an exponent (`strconv.FormatFloat(v, 'e', -1, 64)`), a string in single quotes, a blob in hex, and NULL as `NULL`. It writes NOT NULL only for a column that says it.
  - For every statement, `Parameters()` is SQLite's count of its parameters, which the test reads as the largest index that `sqlite3_bind_int64` takes on the prepared statement.

The layers:

1. `TestCheck` pins the answer for each supported form and each near miss, with its exact message and offset, and checks each with `verify`. It also checks that every keyword that SQLite reads as a name after FROM is supported there. A second subtest checks the select list: a keyword is supported there as the name of a column if SQLite reads it as one, and no other keyword is. SQLite reads a keyword as the name of a column when it gives `no such column: kw` for it on a table without such a column. That holds for 84 keywords.
2. `TestCheck_corpus`, `TestCheck_corpusMutants`, and `TestCheck_grammar` run `verify` on the corpus of [`internal/sql/corpus`](../corpus/README.md), on one mutant of each entry from `parsetest.Mutants`, and on sentences of SQLite's grammar from the generator of [`internal/sql/parsetest`](../parsetest/README.md) and one mutant of each.
3. `TestGenerate` runs the generator in [`stmttest`](stmttest/stmttest.go), from `generate_test.go`. Each statement must be supported as the statement that the generator wrote, offsets included, and pass `verify`.
   - The generator of `stmttest`, which the tests of the engine and the driver use too, writes random statements of the four forms, with names from a pool of plain names, names in double quotes with odd content, names that differ only in case, names close to the reserved ones, and every keyword that SQLite reads as a name in that place; with each of the five types, in any case of its letters; with values of every type, with weight on their edges: integers at and around the limits, with leading zeros and gaps after the sign; reals at zero and -0.0, at the limits, subnormal, hard to round, in every form of a real literal, and in 17 to 40 digits; strings that are empty, hold `''`, look like SQL or a number, have letters of more than one byte, or are about 1,000 bytes long; and blobs that are empty, of zero bytes, or long, with hex digits in either case; in one insert in four a plain `?` in place of the value; with keywords in any case, and with spaces and comments in every gap.
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
   - A real comes from a float64 that `strconv` writes, in one of six forms: as `strconv` writes it, such as `1.5e+00`; without a point and with an upper-case `E`, such as `15E-1`; without an exponent; without an exponent and without a zero before the point, such as `.5`; with leading and trailing zeros; and with an upper-case `E` and the sign of the exponent, such as `1.5E+03`. Its expected value is the float64. That holds only if SQLite rounds those digits to the nearest float, which the 20,000 statements show for these texts.
   - A string comes from a pool and from random runes, and a blob from random bytes.
4. `TestGenerate_mutants` runs `verify` on five one-token mutants of each statement of the generator, and logs the mutants that are still supported, by how they differ from their statement. `TestGenerate_nearMisses` adds what a one-token mutant cannot. It writes each statement of the generator in every form of its kind, 72 for CREATE TABLE, 80 for INSERT, 61 for SELECT, and 8 for DROP TABLE, and requires that none is supported.
   - Most forms add or change a clause, a keyword, or an item, such as WHERE, RETURNING, a column constraint after PRIMARY KEY, or a second row.
   - Forms of the columns: a table without a primary key whose second column takes NULL, a second column with `default null`, a second PRIMARY KEY, NOT NULL twice, a NULL constraint on the primary key, on a column before it, and on a column after it, another constraint after NOT NULL, a table constraint after the columns, a column and `*` in one select list, and an alias of a column.
   - Forms of the type: a type in double quotes, single quotes, or brackets, `any`, `varchar`, `double`, a type twice, and a plural such as `texts`. The forms of CREATE TABLE use the type of the generated statement.
   - Forms of the literal: an unterminated string, `'a''`, two strings without a comma, `x'6'`, `x'6g'`, `x '61'`, `1e`, `1e+`, `1.5.5`, `.`, `1e999`, `-1e999`, `1_000.5`, `0x1p3`, TRUE, `- false`, `"a"`, a cast, `- 'a'`, `-x'01'`, a concatenation, a literal followed by a letter or a quote, and two literals without a comma.
   - Forms of NULL: `+null`, `-null`, `- null` as a second value, `nullif(1, 1)`, `cast(null as int)`, `is null`, `is not null`, `isnull`, `notnull`, and `not null` after the value, `select null from t`, and `is null` and `is not null` in WHERE. Slice 6 made four forms supported: a column without a constraint after the primary key and one before it, and NULL as the only value and as a second value. Each was replaced by a form that is still not supported.
5. `TestCheck_reals` in `real_test.go` checks the value of real literals against SQLite, on texts made to be hard to round: any digits with a point anywhere; a float's exact value halfway to the next one, cut to 15 to 40 significant digits, and one more or one less in its last digit; runs of nines and zeros; texts around the largest float and the smallest ones; leading zeros; and exponents far out of range or of many digits. One text in three comes after a `-`. For each, the pass gives the value that SQLite's `select` gives, bit for bit, or rejects a literal that SQLite reads as an infinity. The test logs for how many texts Go's `strconv.ParseFloat` gives another float than SQLite.
6. `TestAllowList` in `vocabulary_test.go` parses many statements, and requires that a supported statement uses only the node types and fields of the supported forms. Slice 4 added none: its types and its literals are in nodes and fields that slice 1 has. Slice 5 added `NullConstraint` with `Not` and `Null`, `ColumnRef` with `Column`, `ResultColumn.Expr`, and the commas `CreateTable.Commas`, `IDList.Commas`, `ValuesRow.Commas`, and `SimpleSelect.Commas`. Slice 6 added none: the literal NULL is a `Literal`, a column that takes NULL has no constraint, and the NULL constraint, which is not supported, is a `NullConstraint` without `Not`. It reads the node types from the source of `internal/sql/ast` and their fields through reflection, so the list cannot go stale: every node type, and every field of the node types of the supported forms, must occur in some statement of the test, so that a statement that has one outside them is seen to be not supported. The test sees types and fields, not how many there are or what their tokens say: `strict, without rowid, strict` and `values (+1)` use only the types and fields of the supported forms. The hand-written cases and the check of the meaning cover those. `vocabulary_internal_test.go` checks that every node type has a name, and `TestRecognize` there gives the recognizer trees that no text gives, for its paths that SQLite's grammar never reaches.
7. `FuzzCheck` runs `verify` on any text, and `FuzzGenerate` on a statement of the generator and a mutant of it. `FuzzLiteral` takes any text as the value of an insert and, if the pass supports it, compares its value with what SQLite's `select` gives. One of its seeds is NULL. `make fuzz` runs all three.
8. [`internal/sql/mutation`](../mutation/README.md) runs mutation testing of this package.

Under `-short`, the race detector, or the address sanitizer, the tests on the corpus, the grammar, the generator, and the real literals use a tenth of their texts.

## Results

On the code as it is now, with the default flags:

| Layer | Texts | Rejected before parsing | Syntax error | Syntax error where SQLite does not say | Not supported | Not supported where SQLite does not say | No statement | Supported |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Corpus, and the parts before a byte rejected before parsing | 183,694 | 44,460 | 52,938 | 62 | 66,106 | 237 | 6,437 | 13,454 |
| One mutant of each corpus entry, and the parts before such a byte | 168,292 | 29,535 | 120,774 | 78 | 11,470 | 178 | 5,995 | 262 |
| 20,000 sentences of the grammar and a mutant of each | 40,000 | 0 | 12,996 | 2,356 | 20,250 | 1,479 | 2,895 | 24 |
| Five mutants of each of 20,000 statements of the generator | 100,000 | 0 | 78,422 | 77 | 10,435 | 305 | 0 | 10,761 |
| Near misses of 2,000 statements of the generator | 113,477 | 0 | 11,489 | 0 | 99,046 | 2,942 | 0 | 0 |

The two columns "where SQLite does not say" are the texts that the oracle excuses from the comparison with SQLite: SQLite stopped before the end of the statement for a reason that is not the grammar, so nothing holds the pass's answer to SQLite's there. With the default flags and all the texts, the tests require the numbers of both columns exactly, each on its own (`excused` in `check_test.go`). A change of the oracle or of the pass that excuses more texts, or that moves a text from one column to the other, fails them, and the numbers may be changed only on purpose, with the reason.

The 13,454 supported texts of the corpus are 4 CREATE TABLE, 191 DROP TABLE, 12,667 INSERT, and 592 SELECT, each checked in SQLite. Before slice 6 they were 12,736, with 3 CREATE TABLE and 11,950 INSERT: slice 6 added 717 INSERT with NULL as a value and 1 CREATE TABLE with a column that takes NULL. Before slice 5 they were 1,198, with 654 INSERT and 350 SELECT. Most of the 11,296 INSERT that slice 5 added have no list of columns or several values, and the 242 SELECT that it added name columns. The 471 INSERT more than before slice 4 have a real, a string, or a blob as their value. The two that were supported before the empty name was rejected are `DROP TABLE ""` and `INSERT INTO ""("") VALUES(1)`.

The supported mutants of the generator are the same statement with a semicolon or a comment changed, or the same kind of statement with one of these differences:

- Another name of a table or a column, which can be a keyword from the pool of the mutants.
- Another number of columns or of values.
- Another result column, or a name where the statement has `*`.
- The sign of a value dropped.
- A value replaced by a `?`, or a `?` by a value.
- A value of another kind, where the mutant took a literal from the mutants' pool, such as `x'00'` or `1.5`, in place of the value.

None is another kind of statement. The generator of the near misses writes no `?`, since the forms of the near misses write the value themselves. `TestGenerate` checks 20,000 statements of the generator, each as the statement it wrote.

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

`TestCheck_reals` checks 100,000 texts of real literals: SQLite reads 81,351 as reals and 18,649 as infinities, and the pass answers every one as SQLite gives it. For 6,998 of the texts Go's `strconv.ParseFloat` gives another float than SQLite.

Fuzzing, before slice 4: `FuzzCheck` made 202,673,795 executions in 40 minutes on 6 workers, and on the code after the review 87,983,456 in 30 minutes on 4 workers. `FuzzGenerate` made 11,418,100 executions in 15 minutes on 4 workers; each checks a statement of the generator and its mutant in SQLite. None of the runs found anything. On the code of slice 4, short local runs found nothing: `FuzzLiteral` made 7,192,877 executions in 1 minute, `FuzzCheck` 4,915,832 in 45 seconds, and `FuzzGenerate` ran 30 seconds. CI runs them longer.

Mutation testing, before slice 4, on 2026-11-18 after `StartedTable` moved to `stmttest`, which is not mutated: of 896 mutants, 117 do not compile, 766 are killed, 12 time out, and 1 is equivalent; none is unexplained. Before, with `StartedTable` in the package, of 1,122 mutants, 123 did not compile, 970 were killed, 14 timed out, and 15 were equivalent, 14 of them in `StartedTable`. See [`internal/sql/mutation`](../mutation/README.md#the-pass-after-the-parser). Mutation testing of the code of slice 4 is for a later task. So are mutation testing and long fuzz runs of the code of slice 5. Mutation testing and long fuzz runs of the code of slice 6 are for a later task too.

Statement coverage is 99.0% under `-short`, measured with the code of slice 6. The generated tests do not reach the panic of `CreateTable.Key()` for a table without a primary key, which the pass never gives; the unit test `TestCreateTable_Key` covers it. The places that are not covered:

- The defaults of `literalName` and `unaryName` for a kind of token that the grammar never puts there.
- The name of a node for one of its own tokens whose field has no name, which no offset reaches.
- The empty name of the zero `Type`.
- The panic for a blob token that is not hex. The tokenizer gives a blob only for an even number of hex digits.
- The `isValue` markers of the five value types.

### Run times

Measured on an Apple M4 with 10 cores while other tests ran; the number under the address sanitizer is from a Linux container in Docker on the same machine.

- `go test ./internal/sql/stmt` takes about 70 s with the code of slice 5. Before slice 5 it took about 25 s, `-short` about 3 s, and `-race` about 11 s.
- Under the address sanitizer it took about 43 s before slice 4, and `make test-asan` passed. It is not measured again since.
- `BenchmarkCheck` checks the corpus in about 0.3 s, at about 32 MB/s. `BenchmarkCheck_long` shows a time linear in the length from 10,000 to 100,000 result columns and rows, at about 13 MB/s, and faster for 1,000; a long comment goes at several hundred MB/s.
