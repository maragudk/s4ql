# SQL roadmap

A working document on which SQL the engine supports and what comes next. It changes as slices are agreed and built. The rules that do not change are in [spec.md](spec.md).

## Principles

- **Everything is opt-in.** Nothing is supported until it is listed here, all the way down to `select 1`. Anything else is rejected as not supported.
- **Reject, never reinterpret.** Whatever the engine accepts behaves exactly as in SQLite. Legacy behaviour is left behind by refusing it, not by changing what it means.
- **Small slices.** Support grows one small slice at a time. Each slice goes through every layer and is checked against the oracle before the next one starts.

## Restrictions

These hold for every slice unless this document says otherwise.

- All tables are `STRICT`.
- All tables are `WITHOUT ROWID`. Every table therefore has a primary key, and no integer keys are generated.
- Foreign keys are always enforced.
- Double quotes mean identifiers only. A double-quoted string is rejected, and so are `[brackets]` and backticks around names.
- Single quotes mean strings only. A single-quoted string where a name is expected is rejected.
- No `ANY` column type.
- A name is never empty. `""` as the name of a table or a column is rejected, and SQLite accepts it.
- No settings that change behaviour, for now.
- A table whose name starts with `s4ql_` or `sqlite_`, in any case of the letters, is rejected. SQLite reserves `sqlite_` itself, and `s4ql_` is for the tables that s4ql keeps in its cache.
- A table with the name of one of SQLite's built-in virtual tables, such as `json_each` or `pragma_table_info`, is rejected. SQLite reads those tables without a `create table`.
- UTF-8 only. SQL text, identifiers, and text values are always valid UTF-8. A statement whose text is not valid UTF-8 is rejected before it is parsed, and so is a parameter or an expression that would produce text that is not valid UTF-8. SQLite accepts such text.
- No NUL byte in SQL text. A statement whose text has a NUL byte is rejected before it is parsed. SQLite reads such text up to the NUL byte and ignores the rest.

Rejected for now, to be decided later: upserts, and join variants such as comma joins and `NATURAL JOIN`.

## Milestones

A milestone is a goal that several slices lead to. It helps to choose which slice comes next.

- **The first application on s4ql: [glue](https://www.maragu.dev/glue).** `maragu.dev/glue` runs on s4ql in place of SQLite. Markus on 2026-11-19: "a nice second milestone: get maragu.dev/glue working on this storage instead of SQLite." The SQL that it needs and s4ql does not have yet is in [the research note on glue](research/glue-on-s4ql.md). Slices 5 to 9 lead to it. After slice 9 comes the rest of what glue needs, which is not agreed as slices yet.

## Slices

### Slice 1: one integer column

Status: built. The engine is checked against SQLite, and the `database/sql` driver runs these statements, without parameters, which slice 2 added, and without `db.BeginTx`, which slice 3 added.

```sql
create table t1 (v1 int primary key) strict, without rowid;

select * from t1;

insert into t1 (v1) values (1);

select * from t1;

drop table t1;
```

Included, without adding syntax:

- Any table name, column name, and integer value, in exactly these statement shapes: one `int primary key` column, `select *` from one table, a single-row `insert` that names the column, and `drop table` with one table name. An integer value is a decimal integer literal, with at most one `-` in front, that fits a signed 64-bit integer. A `+` sign, a hex literal, a digit separator, and a number outside 64 bits are not supported yet.
- Several rows and several tables. `select *` returns rows in primary-key order, as SQLite does for these tables.
- The errors these statements can produce, matching SQLite's: inserting a key twice, creating a table that exists, reading, writing, or dropping a table that does not exist, and inserting into a column that does not exist.
- Each statement is its own transaction.
- Surviving a restart: with an empty cache, replaying the log from S3 gives the same answers.

Everything else is rejected, including a second column, `integer` spelled out, `where`, `begin`, and `drop table if exists`.

### Slice 2: parameters

Status: built. The statement pass and the engine are checked against SQLite, and the `database/sql` driver runs an insert with an argument, directly and through a prepared statement. The generated tests make arguments: one insert in three has a `?`, and some calls have none, one too many, one of a type that is not supported, or one with a name. The engine's differential test compares every other call with SQLite and checks those 717 of its 12,000 statements against s4ql's rule; the driver's compares 8,000 calls with mattn/go-sqlite3, also prepared statements run several times, and checks 515 against the rule. The simulation checks that each commit holds the arguments that its caller gave, and finds a planted bug that annotates a wrong one. Mutation testing of the new code left no mutant unexplained.

```go
db.ExecContext(ctx, "insert into t1 (v1) values (?)", 1)
```

Included, without adding syntax:

- A plain `?` in the place where slice 1 has the integer literal of an `insert`. Nowhere else: the other statements of slice 1 have no value.
- An argument that is a Go integer and fits a signed 64-bit integer: what `database/sql` turns into an `int64`.
- Both directly, with `db.ExecContext` and its arguments, and through a prepared statement, with `db.PrepareContext` and then `stmt.ExecContext`, any number of times with different arguments.
- The number of arguments must equal the number of `?` in the text. Any other number is an error, the same for a direct call and a prepared one, and the statement does not run. This also holds for a text with no `?` that is given an argument. SQLite would bind NULL for a missing one; s4ql does not. The error is an ordinary error of s4ql's, whose message starts with `s4ql: ` and says both numbers, and not "not supported".
- The commit object keeps the arguments in the annotation of the statement, by position, next to its text.
- The errors of slice 1, matching SQLite's: a key inserted twice through a parameter gives the same error as with a literal.
- Surviving a restart: with an empty cache, replaying the log gives the same rows for rows inserted through parameters.

Rejected as not supported, where SQLite would accept it:

- The other forms of a parameter: `?1`, `:name`, `@name`, `$name`, and an argument made with `sql.Named`.
- An argument of any other type: nil, a string, a float, a bool, a `[]byte`, a `time.Time`. The message says which type.
- A parameter inside an expression, such as `values (-?)`, or in any other place.

The text is judged first, then the arguments, then the instance: a syntax error is SQLite's whatever the arguments are, and an error of the arguments does not depend on whether the instance serves.

### Slice 3: transactions

Status: built. The engine is checked against SQLite: pinned and generated sequences of statements run in transactions on both sides, SQLite's between `begin immediate` and `commit` or `rollback`, or under `pragma query_only`. The `database/sql` driver runs transactions, tested through `database/sql`. The differential test of the driver, `internal/sqlite/crosscheck/drivercheck`, runs generated transactions through `database/sql` on both sides, against SQLite through `mattn/go-sqlite3`, whose transactions begin with `begin immediate` and whose read-only ones run under `pragma query_only`: 200 sequences of 40 calls make 499 transactions, 342 write and 157 read-only, of which 359 commit and 140 roll back, on pinned connections and through the pool. The result of each commit and roll back and every table after it are compared with SQLite, and so is every call in them, except the 139 that s4ql refuses by its own rule (67 isolation levels, 44 statements while rows of the transaction are open, and 28 second begins on a connection), which are checked against the rule, with their numbers pinned. The engine's simulation runs transactions: one turn of a caller in four is a write or a read-only transaction of two to six statements, under the faults of the store, crashes, and closes. It checks that a commit holds exactly the statements of its transaction that succeeded and that write, in order, that nothing of a transaction that rolled back is in the log, that every statement gives what SQLite gives on the state of its transaction's beginning with the transaction's own writes, and that two write transactions never overlap. 1,000 seeds of each workload made 3,974 commits of more than one statement, with no violation, and the simulation finds three planted bugs of transactions. The core has a property test of its savepoints, against a model and a replay of the log. Mutation testing of the code of transactions, 628 mutants of the engine and 101 of the savepoints and checks of serving of the core, left no mutant unexplained: 14 survivors of the first run were gaps in the tests, which new tests close, and 33 mutants are equivalent or out of the reach of any test.

```go
tx, err := db.BeginTx(ctx, nil)
tx.ExecContext(ctx, "insert into t1 (v1) values (?)", 1)
tx.ExecContext(ctx, "insert into t1 (v1) values (?)", 2)
err = tx.Commit()
```

Included, without adding syntax:

- `db.BeginTx(ctx, nil)` and `db.Begin()` begin a write transaction. It takes the place of the single writer when it begins, as SQLite's `begin immediate` does, and holds it until its commit or its roll back. `BeginTx` waits for that place as long as its context allows.
- `db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})` begins a read-only transaction. It reads the state of its beginning, whatever commits meanwhile, and holds up no one.
- The isolation levels `sql.LevelDefault` and `sql.LevelSerializable`, for both kinds of transaction. They are what the transactions give: writers run one after another, and a read-only transaction reads one committed state.
- Everything of slices 1 and 2 inside a transaction: `tx.ExecContext`, `tx.QueryContext`, `tx.QueryRowContext`, `tx.PrepareContext`, and `tx.StmtContext` with a statement prepared on the `db`, with the arguments of slice 2. `tx.Commit` and `tx.Rollback` end the transaction. `conn.BeginTx` on a `sql.Conn` works the same way.
- A write transaction reads its own changes, and nothing outside it sees them before its commit.
- A statement that fails leaves the transaction usable, with the effects of the statements before it, as in SQLite.
- `create table` and `drop table` are transactional: the later statements of the transaction see them, and a roll back undoes them.
- One commit object for each transaction. It holds the statements of the transaction that write and that succeeded, in order, each with its arguments. A transaction that changes nothing writes none.
- A write in a read-only transaction gets SQLite's error `attempt to write a readonly database`, with the code 8 and the extended code 8.
- A commit object that is too large for the store fails the commit with an error that matches the new `s4ql.ErrTooLarge`, and nothing is committed.
- Surviving a restart: with an empty cache, replaying the log gives the same rows and tables for what transactions committed.

Rejected as not supported, where SQLite would accept it:

- The SQL texts `begin`, `commit`, `end`, `rollback`, `savepoint`, and `release`, also inside a transaction, which stays open.
- Every other isolation level, such as `sql.LevelReadCommitted`. Nothing is begun.
- A statement in a transaction while the rows of an earlier query of the transaction are open. For now, this rules out a write inside a loop over the rows of a query: close the rows first.

### Slice 4: the other types

Status: built. The statement pass, the engine, and the `database/sql` driver hold all five types, with literals, arguments, and exact types. The pass reads a real literal by a port of SQLite's own conversion, and agrees with SQLite bit for bit on 100,000 texts that are hard to round (81,351 reals and 18,649 infinities), on 6,998 of which Go's `strconv.ParseFloat` gives another float. The generated tests make tables of all five types, with literals and arguments of every kind, weighted to the edges, values of another type than their column, and arguments that no column holds. The engine's differential test compares 12,000 statements with SQLite, half of its sequences with the length limit lowered to 1,000 bytes, and checks 1,152 of them against s4ql's rule (644 with the wrong number of arguments, 140 with an argument that no column holds, 363 with a value of another type than its column, and 5 with a literal longer than the limit); the driver's compares 8,000 calls with mattn/go-sqlite3 and checks 694 against the rule. Both pin how often each case is reached. The simulation runs with typed tables and checks that every insert in the log holds a value of the type of its column, and it finds a planted bug, `convertReal`, which stores a whole real in an `int` column as an integer; no comparison with SQLite can see that bug, since SQLite converts the value in the same way. Mutation testing of the code of slice 4 made 2,627 mutants in five packages: 781 in `stmt`, 1,010 in the engine, 628 in the core, 90 in the format, and 118 in the driver. Of them 513 did not compile, 1,987 were killed, 16 timed out, and 105 are equivalent, each listed with its reason. Six are left open, all in the core: they drop the error of the temporary tables of a transaction, and [todo.md](todo.md) records the test to write. No mutant showed a fault in the code. The fuzz targets `FuzzTypes` and `FuzzTypes_core` give the engine and the core the bytes of their input as the value of an insert into a table of each type, in each form of literal and argument; the second also lowers the length limit and replays the log.

```sql
create table t1 (v1 text primary key) strict, without rowid;

insert into t1 (v1) values ('a');

select * from t1;
```

```go
db.ExecContext(ctx, "insert into t1 (v1) values (?)", "b")
```

Included:

- The column types `int`, `integer`, `real`, `text`, and `blob`, for the one column of a table, which is its primary key.
- Literals: an integer as in slice 1; a real number, such as `1.5`, `.5`, and `1e3`, with at most one `-` in front; a string in single quotes, with `''` for a quote in it; and a blob as `x'6162'`. s4ql reads every real literal to the same 64-bit float as SQLite, bit for bit, by a port of SQLite's own conversion. It rejects none for that reason, only a literal that SQLite reads as an infinity.
- Arguments: a Go integer, a `float64` or `float32`, a `string`, and a `[]byte` that is not nil.
- Exact types. A value must have the type of its column: an integer for `int` and `integer`, a real for `real`, text for `text`, a blob for `blob`. A value of another type is rejected as not supported. This holds also where SQLite converts the value, such as the integer `1` for a `real` column, the text `'1'` for an `int` column, and the real `1.5` for a `text` column, and where SQLite gives an error of its own, such as text for a `blob` column. Markus on 2026-11-19: "Let's stay strict and maybe loosen later".
- Keys are compared as SQLite compares them: text and blobs byte by byte, reals by their number. `select *` returns rows in that order. `0.0` and `-0.0` are the same key, and `-0.0` is stored as `0.0`, as in SQLite.
- The error of a key that exists, matching SQLite's, for every type.
- A text or a blob of up to 1,000,000,000 bytes, SQLite's limit. A longer argument gets SQLite's error. A literal that long gets `string or blob too big` from s4ql, where SQLite gives `statement too long`, which [todo.md](todo.md) records as an open difference.
- The commit object names the type of a column as it was declared, so `int` and `integer` stay apart, and replay declares the column in the same way.
- Everything of slices 1 to 3 with these types: arguments, prepared statements, transactions, and a restart with an empty cache.

Rejected as not supported, where SQLite would accept it:

- A text value with a NUL byte. SQLite stores it, and its own functions stop at the NUL. A blob takes any bytes.
- A text argument that is not valid UTF-8, as the restrictions say.
- A real that is not a number, and an infinite one, as an argument or as a literal that is too large, such as `1e999`. SQLite stores a NaN as NULL.
- `null` and a nil argument, and a nil `[]byte`: a table has no column yet that takes NULL.
- An argument of type `bool` or `time.Time`.
- An integer literal outside 64 bits, which SQLite reads as a real, a hex literal, and a digit separator, as before.

### Slice 5: several columns

Status: built. The statement pass, the engine, and the `database/sql` driver hold tables of one or more columns, `insert` with and without a list of columns, and `select` of names and of `*`, with SQLite's errors in SQLite's order. The engine's `TestEngine_columns` compares 13 shapes of tables, of two and three columns of mixed types with the key at each position, with SQLite, and a table of 2,000 columns, SQLite's limit. The generated tests make tables of one to three columns with the key at any position; inserts with a list in the order of the table and in another, with a column left out, a column that the table does not have, a wrong number of values, a value of another type, and two faults at once; and selects of `*`, of names in another order, of a name twice, and of a name that the table does not have. The engine's differential test compares 12,000 statements with SQLite, half of its sequences with the length limit lowered to 1,000 bytes, and checks 1,254 of them against s4ql's rule (718 with the wrong number of arguments, 197 with an argument that no column holds, 336 with a value of another type than its column, and 3 with a literal longer than the limit); the driver's `TestGenerated` in `internal/sqlite/crosscheck/drivercheck` compares 8,000 calls with mattn/go-sqlite3 and checks 746 against the rule (396 with the wrong number of arguments, 189 with an argument that the driver does not support, and 161 with a value of another type than its column). Both pin how often each case is reached. The simulation checks that every row in the log has one value for each column of its table, of the type of that column, and it finds a planted bug, `listOrder`, which gives the values of an insert to the columns in the order of its list. The replay of the log, which the bug breaks, also finds it. Mutation testing of the new code and fuzz targets of several columns are still to come.

```sql
create table t1 (v1 int primary key, v2 text not null, v3 real not null) strict, without rowid;

insert into t1 values (1, 'a', 1.5);

insert into t1 (v3, v1, v2) values (2.5, 2, 'b');

select v2, v1, v2 from t1;

select * from t1;
```

```go
db.ExecContext(ctx, "insert into t1 (v1, v2, v3) values (?, ?, ?)", 3, "c", 3.5)
```

Included:

- A table with one or more columns, each of one of the five types of slice 4. Exactly one column is the primary key. It is declared on the column, and it can be at any position. Every table is still `strict, without rowid`.
- Every column that is not the primary key is declared `not null`. The primary-key column can also say `not null`. SQLite accepts it, and it changes nothing.
- `insert into t1 values (...)` gives one value for each column, in the order of the table. `insert into t1 (v2, v1) values (...)` names the columns in any order. Each value is a literal or a `?`, with the exact type of its column, as in slice 4.
- A column that is left out of the list of an `insert` has no value. It gets SQLite's error `NOT NULL constraint failed: t1.v2`.
- The other errors of these statements match SQLite's: a wrong number of values, an unknown column, a column named twice in a table, and more columns than SQLite allows.
- The errors of an `insert` come in SQLite's order. The names and the number of values come first, then a text or a blob that is too long, then the refusal of a read-only transaction. After these comes `NOT NULL constraint failed`, for the first column without a value in the order of the table. Next is "not supported" for a value of another type than its column, for the first such column in the order of the table. A row that is too long comes next, also when its key exists, because SQLite builds the record of the row before it looks for its key. A key that exists comes last.
- `select` names columns in any order, and it can name a column more than once. `select *` gives all columns in the order of the table. An unknown column gets SQLite's error.
- Rows come in the order that SQLite gives them.
- The commit format does not change: a row already holds several values.
- Everything of slices 1 to 4 with several columns: arguments, prepared statements, transactions, and a restart with an empty cache.

Rejected as not supported, where SQLite would accept it:

- A column that is not the primary key and is not declared `not null`. `null` comes in slice 6.
- A column named twice in the column list of an `insert`, such as `insert into t1 (v1, v2, v2)`. SQLite accepts it and uses the first value.
- `null` as a value.
- A default value for a column.
- A table constraint, such as `primary key (v1, v2)`, and a primary key of several columns.
- `where`, `update`, and `delete`.
- An expression or an alias in the select list, such as `v1 + 1` or `v1 as a`.

### Slice 6: `null`

Status: built. The statement pass, the engine, the storage core, and the `database/sql` driver hold columns that take null, with null given by the literal, by a nil argument, and by a column that an `insert` leaves out, and with SQLite's errors in SQLite's order. The commit format got the field `nullable` on a column, and the cache went to version 2. The engine's `TestEngine_columns` compares 44 shapes of tables with SQLite, of two and three columns with the key at each position and with none, some, or all of the other columns taking null, and a table of 2,000 columns. The generated tests make tables in which some, all, or no columns outside the key take null, and null as a literal and as a nil argument, also beside a key that exists and a value of another type, in the key and in a column that does not take it, and for a column that a list leaves out. The engine's differential test compares 12,000 statements with SQLite, half of its sequences with the length limit lowered to 1,000 bytes, and checks 1,270 of them against s4ql's rule (703 with the wrong number of arguments, 220 with an argument that no column holds, 339 with a value of another type than its column, and 8 with a literal longer than the limit); the driver's `TestGenerated` in `internal/sqlite/crosscheck/drivercheck` compares 8,000 calls with mattn/go-sqlite3 and checks 787 against the rule (426 with the wrong number of arguments, 200 with an argument that the driver does not support, and 161 with a value of another type than its column). It gives nil in ten kinds that `database/sql` turns into nil, such as a nil pointer and a `sql.Null*` value that is not valid, and reads each row into an `any`, into `sql.Null*` values, and into pointers. Both pin how often each case is reached. The simulation of the engine requires null values in the log of each workload, and checks that each is in a column that takes null. It finds a planted bug, `zeroLeftOut`, which gives a column that takes null and that a list leaves out the zero value of its type, mostly by a query that gives 0 where SQLite gives null. The simulation of the core finds a planted bug, `nullZero`, which writes each null of a row in the commit object as the zero value of its column's type. Mutation testing of the code of slices 5 and 6 is still to come.

```sql
create table t1 (v1 int primary key, v2 text) strict, without rowid;

insert into t1 values (1, null);

insert into t1 (v1) values (2);

select * from t1;
```

```go
db.ExecContext(ctx, "insert into t1 (v1, v2) values (?, ?)", 3, nil)
```

Included:

- A column that is not the primary key can be declared without `not null`. It then takes null.
- The primary-key column is never null, because the table is `without rowid`. A null for it gets SQLite's error `NOT NULL constraint failed: t1.v1`.
- The literal `null` as a value in an `insert`.
- A nil argument: a Go `nil`, and whatever `database/sql` turns into nil before it reaches the driver, such as a nil pointer and a `sql.Null*` value that is not valid. `database/sql` hands the driver nil for a nil pointer of any type, also `(*[]byte)(nil)`, for a `driver.Valuer` that gives nil, such as a `sql.NullString` or a `sql.Null[T]` that is not valid, and for a nil pointer to a `Valuer` with a value receiver; it hands a nil `[]byte`, which stays rejected, for `[]byte(nil)`, a pointer to one, a valid `sql.Null[[]byte]` whose value is nil, and `sql.RawBytes(nil)`.
- A column that is left out of the list of an `insert` gets null if it takes null.
- A null for a column that is declared `not null` gets SQLite's error `NOT NULL constraint failed: t1.v2`. The error names the first column, in the order of the table, that does not take null and gets one. A null that the `insert` gives and a column that it leaves out are one case there. The error comes before the refusal of a value of another type, and before the error of a key that exists.
- A null goes into a column of any type. The exact types of slice 4 hold for every value that is not null.
- A null comes back as nil. It scans into a `sql.Null*` value, a pointer, and an `any`.
- The commit format gets the field `nullable` on a column, which says that the column takes null. It already has a null value.
- Everything of slices 1 to 5 with null: arguments, prepared statements, transactions, and a restart with an empty cache.

Rejected as not supported, where SQLite would accept it:

- A nil `[]byte`, as in slice 4. It does not say whether it means null or the empty blob.
- `default`.
- `is null` and `is not null`. They come with `where`, in slice 7.
- A null after a sign, `-null` or `+null`. SQLite reads both as null.
- The column constraint `null`, as in `v2 text null`. SQLite reads it as no constraint, so the column takes null with it as without it.
- A null anywhere but as a value of an `insert`, such as `select null from t1`.

### Slice 7: `where` on `select`

Status: built. The statement pass, the engine, and the `database/sql` driver run a `where` on a `select`. The plan reads by the primary key where the conditions allow it: no row at all for a comparison with null or for `key is null`, a point lookup for `key = value`, a scan between the bounds that `<`, `<=`, `>`, and `>=` put on the key, and a scan of every row otherwise. The filter tests every condition on every row that the scan gives, so the use of the key can only leave out rows that a condition rejects. The generated tests write selects with a `where`, with values that the inserts gave and values beside them. The engine's differential test runs every such select both by the key and with a scan of every row, compares both with SQLite, and checks that the plan reads by the key as the rule says, which no comparison of rows can see. The fuzz targets `FuzzWhere` and `FuzzWhere_core` build a `where` from their input, and read and compare it both ways too. The driver's generated test compares the selects with mattn/go-sqlite3, by the key only, because the driver has no option to read every row. In the simulation, every second engine of a node reads every row, so the comparisons with SQLite see both ways, also across crashes and closes. It finds a planted bug, `openBound`, which reads a closed bound of the key as open and leaves out the row whose key is the bound. Mutation testing of the code of slice 7 is still to come.

```sql
select * from t1 where v1 = 1;

select v2 from t1 where v1 >= 2 and v1 < 10 and v2 is not null;
```

```go
db.QueryContext(ctx, "select * from t1 where v2 = ?", "a")
```

Included:

- `select ... from t1 where <condition>`.
- A condition compares a column with a literal or a `?`, with `=`, `==`, `!=`, `<>`, `<`, `<=`, `>`, or `>=`. The column is on the left.
- The value of a comparison is a literal of slice 4, `null`, or a `?`.
- `=` and `==` mean the same, and so do `!=` and `<>`.
- The conditions `v2 is null` and `v2 is not null`.
- Conditions joined with `and`. Several conditions can test the same column, as in `v1 >= 2 and v1 < 10`.
- Exact types. The value has the type of its column, as in slice 4. A value of another type is rejected as not supported. This holds also where SQLite converts the value, such as the text `'1'` for an `int` column and the integer `1` for a `real` column.
- A comparison with null, such as `v2 = null` or `v2 = ?` with a nil argument, is unknown, as in SQLite, and gives no rows.
- Values compare as SQLite compares them. Text compares by its bytes, which is SQLite's `binary` collation.
- Rows come in the order that SQLite gives them.
- Everything of slices 1 to 6 with `where`: arguments, prepared statements, transactions, and a restart with an empty cache.

Further details are decided when the slice is built, and recorded here. The forms of the `where`, token for token, are in [internal/sql/stmt/README.md](../internal/sql/stmt/README.md).

- The errors of a `select` with `where` come in SQLite's order. An unknown table comes first, then an unknown column of the select list, then the columns of the conditions, in the order of the text. After these comes `string or blob too big` for a value of a condition that is too long. Last is "not supported" for a value of another type than its column, for the first such condition in the order of the text.
- The first comparison of an unknown column gets SQLite's error, such as `no such column: v8`. An unknown column under `is null` or `is not null` does not stop SQLite at once: a later such test replaces the name, and the next comparison ends the search with it. So `v8 is null and v9 is null` names `v9`, and `v8 is null and v9 = 1` names `v8`, in a table that has neither.
- The refusal of a value of another type names the value and the column: `s4ql: not supported: a string literal compared with the column t1.v1 of type INT`, or `s4ql: not supported: argument 1, which is text, compared with the column t1.v1 of type INT`. A null has no type, and is never refused.
- A `select` by the key, such as `v1 = 1`, reads only the row of that key. One by a range of the key, such as `v1 >= 2 and v1 < 10`, reads only the rows of that range. A `where` that does not bound the key reads every row of the table.
- The rows are the same however they are read, and they come in the order of the key.

Rejected as not supported, where SQLite would accept it:

- `update` and `delete`, with or without `where`. They come in slice 8.
- A literal or a `?` on the left of a comparison, such as `1 = v1`.
- `or`, `not`, and parentheses.
- A column compared with another column.
- A column with the name of its table, such as `t1.v1 = 1`.
- Expressions and functions, such as `v1 + 1 = 2` and `lower(v2) = 'a'`.
- `in`, `between`, `like`, and `glob`.
- `collate`.
- `is` with anything but `null`, such as `v1 is 1`, `v1 is true`, and `v1 is distinct from 1`.
- `isnull`, `notnull`, and `not null` after a column. SQLite reads them as `is null` and `is not null`.
- `order by` and `limit`.
- A `where` deeper than SQLite's limit of 1000 levels for an expression, which SQLite refuses while it parses. The most is 999 conditions, or 998 if one of the first two has a sign before its value.

### Slice 8: `update` and `delete`

Status: agreed on 2026-11-19, not built.

```sql
create table t1 (v1 int primary key, v2 text, v3 real not null) strict, without rowid;

update t1 set v2 = 'b', v3 = 2.5 where v1 = 1;

update t1 set v2 = null;

delete from t1 where v1 >= 10;

delete from t1;
```

```go
res, err := db.ExecContext(ctx, "update t1 set v2 = ? where v1 = ?", "c", 2)
n, err := res.RowsAffected()
```

Included:

- `update t1 set v2 = <value>, v3 = <value>` and `delete from t1`, each with or without the `where` of slice 7.
- A value in `set` is a literal, `null`, or a `?`. A value that is not null has the exact type of its column, as in slice 4.
- `null` only for a column that takes null. For any other column, it gets SQLite's error `NOT NULL constraint failed: t1.v2`.
- An `update` can change the primary key, as in SQLite. If a key then exists twice, the statement gets SQLite's error of a key that exists, and it changes nothing.
- An unknown column in `set` gets SQLite's error.
- `RowsAffected` gives the number of rows that SQLite reports as changed. That is every row that the `where` matches, also when an `update` sets a value that the row already has.
- The commit format does not change: it already has rows to write and keys to delete.
- Everything of slices 1 to 7 with `update` and `delete`: arguments, prepared statements, transactions, and a restart with an empty cache.

Further details are decided when the slice is built, and recorded here.

Rejected as not supported, where SQLite would accept it:

- An expression in `set`, such as `v1 = v1 + 1`.
- `returning`.
- `update ... from`.
- `set (v2, v3) = (...)`.
- `or replace` and the other conflict clauses.
- `limit` and `order by` on `update` and `delete`.

### Slice 9: `if not exists` and `if exists`

Status: agreed on 2026-11-20, not built.

```sql
create table if not exists t1 (v1 int primary key, v2 text) strict, without rowid;

drop table if exists t1;
```

Included:

- `create table if not exists t1 (...)`: when no table `t1` exists, it does what `create table` does. When a table `t1` exists, the statement does nothing and gives no error, whatever the columns of the existing table are, as in SQLite.
- `drop table if exists t1`: when no table `t1` exists, nothing happens and there is no error. When it exists, it does what `drop table` does.
- A statement that does nothing writes nothing to the log.
- An error that the declaration would give is still given by SQLite's rules when the table exists, for example a syntax error in the column list. What SQLite does with a declaration that is valid in syntax but not supported by s4ql, when the table exists, is decided when the slice is built, by SQLite's behaviour and the rule of the statement pass, and recorded in [the todo](todo.md).
- The commit format does not change.
- Everything of slices 1 to 8 with the two forms: transactions, prepared statements, and a restart with an empty cache.
- The reason: the migration tool of glue creates its migrations table with `create table if not exists`.

Further details are decided when the slice is built, and recorded here.

Rejected as not supported, where SQLite would accept it:

- `create temp table`.
- `create table ... as select`.
- Everything else that slices 1 to 8 reject in a declaration.
